package server

import (
	"context"
	"io"
	"log/slog"
	"sync"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/colinrgodsey/goREgo/pkg/cluster"
	"github.com/colinrgodsey/goREgo/pkg/scheduler"
	"github.com/colinrgodsey/goREgo/pkg/storage"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	// forwardedMetadataKey marks requests that have been forwarded from another node.
	// Forwarded requests must execute locally to prevent forwarding loops.
	forwardedMetadataKey = "x-gorego-forwarded"
)

// ExecutionServer implements the REAPI Execution service.
type ExecutionServer struct {
	repb.UnimplementedExecutionServer
	scheduler      *scheduler.Scheduler
	actionCache    storage.ActionCache
	clusterManager *cluster.Manager
	tracer         trace.Tracer
	logger         *slog.Logger

	// Connection pool for peer connections
	connMu sync.RWMutex
	conns  map[string]*grpc.ClientConn
}

// NewExecutionServer creates a new ExecutionServer.
func NewExecutionServer(sched *scheduler.Scheduler, ac storage.ActionCache, cm *cluster.Manager) *ExecutionServer {
	return &ExecutionServer{
		scheduler:      sched,
		actionCache:    ac,
		clusterManager: cm,
		tracer:         otel.Tracer("gorego/pkg/server/execution"),
		logger:         slog.Default().With("component", "execution"),
		conns:          make(map[string]*grpc.ClientConn),
	}
}

// getOrCreateConn returns a gRPC connection to the given address.
func (s *ExecutionServer) getOrCreateConn(ctx context.Context, addr string) (*grpc.ClientConn, error) {
	s.connMu.RLock()
	conn, ok := s.conns[addr]
	s.connMu.RUnlock()
	if ok {
		return conn, nil
	}

	s.connMu.Lock()
	defer s.connMu.Unlock()

	// Double-check after acquiring write lock
	if conn, ok := s.conns[addr]; ok {
		return conn, nil
	}

	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, err
	}

	s.conns[addr] = conn
	return conn, nil
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

	// Check if we should forward to a peer (cluster mode)
	// Only consider forwarding if this request wasn't already forwarded to us.
	// This prevents forwarding loops between nodes.
	isForwarded := false
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get(forwardedMetadataKey); len(vals) > 0 {
			isForwarded = true
			span.SetAttributes(attribute.Bool("received_forwarded", true))
		}
	}

	if s.clusterManager != nil && !isForwarded {
		if peer := s.clusterManager.SelectBestPeer(); peer != nil {
			span.SetAttributes(
				attribute.Bool("forwarded", true),
				attribute.String("forward.peer", peer.Name),
			)
			s.logger.Debug("forwarding request to peer",
				"peer", peer.Name,
				"grpc_address", peer.GRPCAddress,
			)
			return s.forwardExecute(ctx, req, stream, peer)
		}
	}

	span.SetAttributes(attribute.Bool("forwarded", false))

	// Enqueue for local execution
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

// forwardExecute forwards an Execute request to a peer node.
func (s *ExecutionServer) forwardExecute(ctx context.Context, req *repb.ExecuteRequest, stream repb.Execution_ExecuteServer, peer *cluster.Peer) error {
	conn, err := s.getOrCreateConn(ctx, peer.GRPCAddress)
	if err != nil {
		s.logger.Error("failed to connect to peer", "peer", peer.Name, "error", err)
		// Fall back to local execution
		return s.executeLocally(ctx, req, stream)
	}

	// Mark this request as forwarded to prevent the peer from forwarding it again.
	// This ensures single-hop forwarding only.
	forwardCtx := metadata.AppendToOutgoingContext(ctx, forwardedMetadataKey, "1")

	client := repb.NewExecutionClient(conn)
	peerStream, err := client.Execute(forwardCtx, req)
	if err != nil {
		s.logger.Error("failed to forward to peer", "peer", peer.Name, "error", err)
		// Fall back to local execution
		return s.executeLocally(ctx, req, stream)
	}

	// Proxy responses from peer to client
	for {
		op, err := peerStream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(op); err != nil {
			return err
		}
	}
}

// executeLocally runs the action on the local node.
func (s *ExecutionServer) executeLocally(ctx context.Context, req *repb.ExecuteRequest, stream repb.Execution_ExecuteServer) error {
	op, err := s.scheduler.Enqueue(ctx, req.ActionDigest, req.SkipCacheLookup)
	if err != nil {
		return err
	}

	updates, unsubscribe := s.scheduler.Subscribe(op.Name)
	defer unsubscribe()

	if err := stream.Send(op); err != nil {
		return err
	}

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

	// Check if operation belongs to a different node (cluster mode)
	if s.clusterManager != nil {
		nodeID, _ := scheduler.ParseOperationNodeID(req.Name)
		localNodeID := s.clusterManager.NodeID()

		if nodeID != "" && nodeID != localNodeID {
			// Operation belongs to a different node, forward the request
			span.SetAttributes(
				attribute.Bool("forwarded", true),
				attribute.String("forward.node", nodeID),
			)

			peer := s.clusterManager.GetPeerByNodeID(nodeID)
			if peer == nil {
				return status.Errorf(codes.NotFound, "node %s not found in cluster", nodeID)
			}

			return s.forwardWaitExecution(ctx, req, stream, peer)
		}
	}

	span.SetAttributes(attribute.Bool("forwarded", false))

	// Verify operation exists locally
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

// forwardWaitExecution forwards a WaitExecution request to the peer that owns the operation.
func (s *ExecutionServer) forwardWaitExecution(ctx context.Context, req *repb.WaitExecutionRequest, stream repb.Execution_WaitExecutionServer, peer *cluster.Peer) error {
	conn, err := s.getOrCreateConn(ctx, peer.GRPCAddress)
	if err != nil {
		return status.Errorf(codes.Unavailable, "failed to connect to node %s: %v", peer.Name, err)
	}

	client := repb.NewExecutionClient(conn)
	peerStream, err := client.WaitExecution(ctx, req)
	if err != nil {
		return err
	}

	// Proxy responses from peer to client
	for {
		op, err := peerStream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(op); err != nil {
			return err
		}
	}
}
