package scheduler

import (
	"context"
	"fmt"
	"sync"
	"time"

	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
)

// OperationState represents the current state of an operation.
type OperationState int

const (
	StateQueued OperationState = iota
	StateExecuting
	StateCompleted
)

// Task represents a pending action execution request.
type Task struct {
	OperationID  string
	ActionDigest *repb.Digest
	SkipCache    bool
	QueuedAt     time.Time
}

// OperationStatus holds the status of an operation.
type OperationStatus struct {
	Name      string
	State     OperationState
	Metadata  *repb.ExecuteOperationMetadata
	Result    *repb.ExecuteResponse
	Error     error
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Scheduler manages operations and the task queue.
type Scheduler struct {
	mu          sync.RWMutex
	operations  map[string]*OperationStatus
	subscribers map[string][]chan *OperationStatus

	taskQueue chan *Task
	queueSize int

	// Retention duration for completed operations
	retention time.Duration

	// Node ID prefix for operation IDs (for cluster routing)
	nodeID string

	// Count of currently executing operations
	executingCount int
}

// NewScheduler creates a new Scheduler with the given queue size and node ID.
// The nodeID is used to prefix operation IDs for cluster routing.
func NewScheduler(queueSize int, nodeID string) *Scheduler {
	return &Scheduler{
		operations:  make(map[string]*OperationStatus),
		subscribers: make(map[string][]chan *OperationStatus),
		taskQueue:   make(chan *Task, queueSize),
		queueSize:   queueSize,
		retention:   10 * time.Minute,
		nodeID:      nodeID,
	}
}

// Enqueue creates a new operation and adds it to the task queue.
// Returns RESOURCE_EXHAUSTED if the queue is full.
func (s *Scheduler) Enqueue(ctx context.Context, actionDigest *repb.Digest, skipCache bool) (*longrunning.Operation, error) {
	operationID := uuid.New().String()
	// Prefix with node ID for cluster routing (format: nodeID:uuid)
	if s.nodeID != "" {
		operationID = fmt.Sprintf("%s:%s", s.nodeID, operationID)
	}
	name := fmt.Sprintf("operations/%s", operationID)
	now := time.Now()

	metadata := &repb.ExecuteOperationMetadata{
		Stage:        repb.ExecutionStage_QUEUED,
		ActionDigest: actionDigest,
	}

	task := &Task{
		OperationID:  operationID,
		ActionDigest: actionDigest,
		SkipCache:    skipCache,
		QueuedAt:     now,
	}

	opStatus := &OperationStatus{
		Name:      name,
		State:     StateQueued,
		Metadata:  metadata,
		CreatedAt: now,
		UpdatedAt: now,
	}

	s.mu.Lock()
	s.operations[name] = opStatus
	s.mu.Unlock()

	// Non-blocking send to queue
	select {
	case s.taskQueue <- task:
		// Queued successfully
	default:
		// Queue is full
		s.mu.Lock()
		delete(s.operations, name)
		s.mu.Unlock()
		return nil, status.Error(codes.ResourceExhausted, "execution queue is full")
	}

	return s.toProto(opStatus)
}

// Dequeue returns the next task from the queue, blocking if empty.
func (s *Scheduler) Dequeue(ctx context.Context) (*Task, error) {
	select {
	case task := <-s.taskQueue:
		return task, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// UpdateState updates the state of an operation and notifies subscribers.
func (s *Scheduler) UpdateState(name string, state OperationState, metadata *repb.ExecuteOperationMetadata) {
	s.mu.Lock()
	defer s.mu.Unlock()

	op, ok := s.operations[name]
	if !ok {
		return
	}

	// Track executing count transitions
	if op.State != StateExecuting && state == StateExecuting {
		s.executingCount++
	} else if op.State == StateExecuting && state != StateExecuting {
		s.executingCount--
	}

	op.State = state
	op.Metadata = metadata
	op.UpdatedAt = time.Now()

	s.notifySubscribersLocked(name, op)
}

// Complete marks an operation as completed with the given result.
func (s *Scheduler) Complete(name string, result *repb.ExecuteResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()

	op, ok := s.operations[name]
	if !ok {
		return
	}

	// Track executing count if transitioning from executing
	if op.State == StateExecuting {
		s.executingCount--
	}

	op.State = StateCompleted
	op.Result = result
	op.Metadata.Stage = repb.ExecutionStage_COMPLETED
	op.UpdatedAt = time.Now()

	s.notifySubscribersLocked(name, op)
}

// Fail marks an operation as failed with an error.
func (s *Scheduler) Fail(name string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	op, ok := s.operations[name]
	if !ok {
		return
	}

	// Track executing count if transitioning from executing
	if op.State == StateExecuting {
		s.executingCount--
	}

	op.State = StateCompleted
	op.Error = err
	op.Metadata.Stage = repb.ExecutionStage_COMPLETED
	op.UpdatedAt = time.Now()

	s.notifySubscribersLocked(name, op)
}

// GetOperation returns the current status of an operation.
func (s *Scheduler) GetOperation(name string) (*longrunning.Operation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	op, ok := s.operations[name]
	if !ok {
		return nil, status.Error(codes.NotFound, "operation not found")
	}

	return s.toProto(op)
}

// ListOperations returns all operations.
func (s *Scheduler) ListOperations() ([]*longrunning.Operation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var ops []*longrunning.Operation
	for _, op := range s.operations {
		proto, err := s.toProto(op)
		if err != nil {
			continue
		}
		ops = append(ops, proto)
	}
	return ops, nil
}

// CancelOperation cancels a queued operation.
func (s *Scheduler) CancelOperation(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	op, ok := s.operations[name]
	if !ok {
		return status.Error(codes.NotFound, "operation not found")
	}

	if op.State != StateQueued {
		return status.Error(codes.FailedPrecondition, "can only cancel queued operations")
	}

	op.State = StateCompleted
	op.Error = status.Error(codes.Canceled, "operation cancelled")
	op.Metadata.Stage = repb.ExecutionStage_COMPLETED
	op.UpdatedAt = time.Now()

	s.notifySubscribersLocked(name, op)
	return nil
}

// DeleteOperation removes an operation from the tracker.
func (s *Scheduler) DeleteOperation(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.operations[name]; !ok {
		return status.Error(codes.NotFound, "operation not found")
	}

	delete(s.operations, name)
	return nil
}

// Subscribe returns a channel that receives updates for an operation.
func (s *Scheduler) Subscribe(name string) (<-chan *OperationStatus, func()) {
	ch := make(chan *OperationStatus, 10)

	s.mu.Lock()
	s.subscribers[name] = append(s.subscribers[name], ch)

	// Send current state immediately
	if op, ok := s.operations[name]; ok {
		select {
		case ch <- op:
		default:
		}
	}
	s.mu.Unlock()

	unsubscribe := func() {
		s.mu.Lock()
		defer s.mu.Unlock()

		subs := s.subscribers[name]
		for i, sub := range subs {
			if sub == ch {
				s.subscribers[name] = append(subs[:i], subs[i+1:]...)
				close(ch)
				break
			}
		}
	}

	return ch, unsubscribe
}

func (s *Scheduler) notifySubscribersLocked(name string, op *OperationStatus) {
	for _, ch := range s.subscribers[name] {
		select {
		case ch <- op:
		default:
			// Subscriber not ready, skip
		}
	}
}

func (s *Scheduler) toProto(op *OperationStatus) (*longrunning.Operation, error) {
	metadataAny, err := anypb.New(op.Metadata)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal metadata: %w", err)
	}

	proto := &longrunning.Operation{
		Name:     op.Name,
		Metadata: metadataAny,
		Done:     op.State == StateCompleted,
	}

	if op.State == StateCompleted {
		if op.Error != nil {
			proto.Result = &longrunning.Operation_Error{
				Error: status.Convert(op.Error).Proto(),
			}
		} else if op.Result != nil {
			resultAny, err := anypb.New(op.Result)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal result: %w", err)
			}
			proto.Result = &longrunning.Operation_Response{
				Response: resultAny,
			}
		}
	}

	return proto, nil
}

// CleanupExpired removes completed operations older than retention duration.
func (s *Scheduler) CleanupExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(-s.retention)
	for name, op := range s.operations {
		if op.State == StateCompleted && op.UpdatedAt.Before(cutoff) {
			delete(s.operations, name)
			delete(s.subscribers, name)
		}
	}
}

// Run starts the cleanup goroutine.
func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.CleanupExpired()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// GetPendingTaskCount returns the number of pending tasks (queued + executing).
// This is used by the cluster manager to report load.
func (s *Scheduler) GetPendingTaskCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// queued tasks in channel + currently executing
	return len(s.taskQueue) + s.executingCount
}

// NodeID returns the node ID used for operation ID prefixing.
func (s *Scheduler) NodeID() string {
	return s.nodeID
}

// ParseOperationNodeID extracts the node ID from an operation name.
// Returns the nodeID and the base operation ID.
// Format: operations/nodeID:uuid or operations/uuid (legacy)
func ParseOperationNodeID(operationName string) (nodeID, operationID string) {
	// Strip "operations/" prefix
	name := operationName
	if len(name) > 11 && name[:11] == "operations/" {
		name = name[11:]
	}

	// Check for node ID prefix (nodeID:uuid)
	for i := 0; i < len(name); i++ {
		if name[i] == ':' {
			return name[:i], name[i+1:]
		}
	}

	// No node ID prefix (legacy format)
	return "", name
}
