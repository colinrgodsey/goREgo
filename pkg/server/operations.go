package server

import (
	"context"

	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/colinrgodsey/goREgo/pkg/scheduler"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/emptypb"
)

// OperationsServer implements the Long-Running Operations service.
type OperationsServer struct {
	longrunning.UnimplementedOperationsServer
	scheduler *scheduler.Scheduler
	tracer    trace.Tracer
}

// NewOperationsServer creates a new OperationsServer.
func NewOperationsServer(sched *scheduler.Scheduler) *OperationsServer {
	return &OperationsServer{
		scheduler: sched,
		tracer:    otel.Tracer("gorego/pkg/server/operations"),
	}
}

// GetOperation returns the current status of an operation.
func (s *OperationsServer) GetOperation(ctx context.Context, req *longrunning.GetOperationRequest) (*longrunning.Operation, error) {
	ctx, span := s.tracer.Start(ctx, "operations.GetOperation",
		trace.WithAttributes(attribute.String("operation.name", req.Name)))
	defer span.End()

	op, err := s.scheduler.GetOperation(req.Name)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	return op, nil
}

// ListOperations returns all operations.
func (s *OperationsServer) ListOperations(ctx context.Context, req *longrunning.ListOperationsRequest) (*longrunning.ListOperationsResponse, error) {
	ctx, span := s.tracer.Start(ctx, "operations.ListOperations")
	defer span.End()

	ops, err := s.scheduler.ListOperations()
	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	return &longrunning.ListOperationsResponse{
		Operations: ops,
	}, nil
}

// CancelOperation cancels a queued operation.
func (s *OperationsServer) CancelOperation(ctx context.Context, req *longrunning.CancelOperationRequest) (*emptypb.Empty, error) {
	ctx, span := s.tracer.Start(ctx, "operations.CancelOperation",
		trace.WithAttributes(attribute.String("operation.name", req.Name)))
	defer span.End()

	if err := s.scheduler.CancelOperation(req.Name); err != nil {
		span.RecordError(err)
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// DeleteOperation removes an operation from the tracker.
func (s *OperationsServer) DeleteOperation(ctx context.Context, req *longrunning.DeleteOperationRequest) (*emptypb.Empty, error) {
	ctx, span := s.tracer.Start(ctx, "operations.DeleteOperation",
		trace.WithAttributes(attribute.String("operation.name", req.Name)))
	defer span.End()

	if err := s.scheduler.DeleteOperation(req.Name); err != nil {
		span.RecordError(err)
		return nil, err
	}
	return &emptypb.Empty{}, nil
}
