package server

import (
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/colinrgodsey/goREgo/pkg/scheduler"
	"github.com/colinrgodsey/goREgo/pkg/storage"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ExecutionServer implements the REAPI Execution service.
type ExecutionServer struct {
	repb.UnimplementedExecutionServer
	scheduler   *scheduler.Scheduler
	actionCache storage.ActionCache
	tracer      trace.Tracer
}

// NewExecutionServer creates a new ExecutionServer.
func NewExecutionServer(sched *scheduler.Scheduler, ac storage.ActionCache) *ExecutionServer {
	return &ExecutionServer{
		scheduler:   sched,
		actionCache: ac,
		tracer:      otel.Tracer("gorego/pkg/server/execution"),
	}
}

// Execute starts an action execution.
func (s *ExecutionServer) Execute(req *repb.ExecuteRequest, stream repb.Execution_ExecuteServer) error {
	ctx := stream.Context()
	ctx, span := s.tracer.Start(ctx, "execution.Execute",
		trace.WithAttributes(
			attribute.String("action_digest.hash", req.ActionDigest.Hash),
			attribute.Int64("action_digest.size", req.ActionDigest.SizeBytes),
		))
	defer span.End()

	// Check action cache first (unless skip_cache_lookup is set)
	if !req.SkipCacheLookup {
		actionDigest, err := storage.FromProto(req.ActionDigest)
		if err == nil {
			if result, err := s.actionCache.GetActionResult(ctx, actionDigest); err == nil {
				span.SetAttributes(attribute.Bool("cache.hit", true))
				// Return cached result
				op, err := s.scheduler.Enqueue(ctx, req.ActionDigest, true)
				if err != nil {
					return err
				}
				// Immediately complete with cached result
				s.scheduler.Complete(op.Name, &repb.ExecuteResponse{
					Result:       result,
					CachedResult: true,
				})
				// Get final operation state
				finalOp, err := s.scheduler.GetOperation(op.Name)
				if err != nil {
					return err
				}
				return stream.Send(finalOp)
			}
		}
		span.SetAttributes(attribute.Bool("cache.hit", false))
	}

	// Enqueue for execution
	op, err := s.scheduler.Enqueue(ctx, req.ActionDigest, req.SkipCacheLookup)
	if err != nil {
		span.RecordError(err)
		return err
	}

	span.SetAttributes(attribute.String("operation.name", op.Name))

	// Subscribe to updates
	updates, unsubscribe := s.scheduler.Subscribe(op.Name)
	defer unsubscribe()

	// Send initial state
	if err := stream.Send(op); err != nil {
		return err
	}

	// Stream updates until done
	for update := range updates {
		opProto, err := s.scheduler.GetOperation(op.Name)
		if err != nil {
			continue
		}
		if err := stream.Send(opProto); err != nil {
			return err
		}
		if update.State == scheduler.StateCompleted {
			break
		}
	}

	return nil
}

// WaitExecution waits for an existing execution to complete.
func (s *ExecutionServer) WaitExecution(req *repb.WaitExecutionRequest, stream repb.Execution_WaitExecutionServer) error {
	ctx := stream.Context()
	ctx, span := s.tracer.Start(ctx, "execution.WaitExecution",
		trace.WithAttributes(attribute.String("operation.name", req.Name)))
	defer span.End()

	// Verify operation exists
	op, err := s.scheduler.GetOperation(req.Name)
	if err != nil {
		span.RecordError(err)
		return err
	}

	// If already done, just return
	if op.Done {
		return stream.Send(op)
	}

	// Subscribe to updates
	updates, unsubscribe := s.scheduler.Subscribe(req.Name)
	defer unsubscribe()

	// Stream updates until done
	for update := range updates {
		opProto, err := s.scheduler.GetOperation(req.Name)
		if err != nil {
			continue
		}
		if err := stream.Send(opProto); err != nil {
			return err
		}
		if update.State == scheduler.StateCompleted {
			break
		}
	}

	// Check context for cancellation
	select {
	case <-ctx.Done():
		return status.Error(codes.Canceled, "client cancelled")
	default:
		return nil
	}
}
