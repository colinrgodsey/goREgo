package scheduler

import (
	"context"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
)

func TestParseOperationNodeID(t *testing.T) {
	tests := []struct {
		name            string
		operationName   string
		wantNodeID      string
		wantOperationID string
	}{
		{
			name:            "parses node-prefixed operation",
			operationName:   "operations/node-01:550e8400-e29b-41d4-a716-446655440000",
			wantNodeID:      "node-01",
			wantOperationID: "550e8400-e29b-41d4-a716-446655440000",
		},
		{
			name:            "handles legacy format (no node prefix)",
			operationName:   "operations/550e8400-e29b-41d4-a716-446655440000",
			wantNodeID:      "",
			wantOperationID: "550e8400-e29b-41d4-a716-446655440000",
		},
		{
			name:            "handles complex node ID with dashes",
			operationName:   "operations/my-node-123:abc-def-ghi",
			wantNodeID:      "my-node-123",
			wantOperationID: "abc-def-ghi",
		},
		{
			name:            "handles operation without prefix",
			operationName:   "550e8400-e29b-41d4-a716-446655440000",
			wantNodeID:      "",
			wantOperationID: "550e8400-e29b-41d4-a716-446655440000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotNodeID, gotOperationID := ParseOperationNodeID(tt.operationName)
			if gotNodeID != tt.wantNodeID {
				t.Errorf("ParseOperationNodeID(%q) nodeID = %q, want %q", tt.operationName, gotNodeID, tt.wantNodeID)
			}
			if gotOperationID != tt.wantOperationID {
				t.Errorf("ParseOperationNodeID(%q) operationID = %q, want %q", tt.operationName, gotOperationID, tt.wantOperationID)
			}
		})
	}
}

func TestNewScheduler_WithNodeID(t *testing.T) {
	s := NewScheduler(100, "test-node")

	if s.NodeID() != "test-node" {
		t.Errorf("NodeID() = %q, want %q", s.NodeID(), "test-node")
	}
}

func TestNewScheduler_WithoutNodeID(t *testing.T) {
	s := NewScheduler(100, "")

	if s.NodeID() != "" {
		t.Errorf("NodeID() = %q, want empty", s.NodeID())
	}
}

func TestEnqueue_WithNodeIDPrefix(t *testing.T) {
	s := NewScheduler(100, "test-node")
	ctx := context.Background()

	digest := &repb.Digest{
		Hash:      "abc123",
		SizeBytes: 100,
	}

	op, err := s.Enqueue(ctx, digest, false)
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	// Operation name should contain the node ID prefix
	nodeID, _ := ParseOperationNodeID(op.Name)
	if nodeID != "test-node" {
		t.Errorf("Enqueue() created operation with nodeID = %q, want %q", nodeID, "test-node")
	}
}

func TestEnqueue_WithoutNodeIDPrefix(t *testing.T) {
	s := NewScheduler(100, "")
	ctx := context.Background()

	digest := &repb.Digest{
		Hash:      "abc123",
		SizeBytes: 100,
	}

	op, err := s.Enqueue(ctx, digest, false)
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	// Operation name should not have a node ID prefix
	nodeID, _ := ParseOperationNodeID(op.Name)
	if nodeID != "" {
		t.Errorf("Enqueue() created operation with nodeID = %q, want empty", nodeID)
	}
}

func TestGetPendingTaskCount(t *testing.T) {
	s := NewScheduler(100, "test-node")
	ctx := context.Background()

	// Initially zero
	if count := s.GetPendingTaskCount(); count != 0 {
		t.Errorf("GetPendingTaskCount() = %d initially, want 0", count)
	}

	// Enqueue a task
	digest := &repb.Digest{
		Hash:      "abc123",
		SizeBytes: 100,
	}
	op, err := s.Enqueue(ctx, digest, false)
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	// Should have 1 pending (in queue)
	if count := s.GetPendingTaskCount(); count != 1 {
		t.Errorf("GetPendingTaskCount() = %d after enqueue, want 1", count)
	}

	// Dequeue the task
	task, err := s.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue() error = %v", err)
	}
	_ = task

	// Should have 0 pending now (dequeued but not executing yet)
	if count := s.GetPendingTaskCount(); count != 0 {
		t.Errorf("GetPendingTaskCount() = %d after dequeue, want 0", count)
	}

	// Update state to executing (pass non-nil metadata to avoid overwriting)
	s.UpdateState(op.Name, StateExecuting, &repb.ExecuteOperationMetadata{
		Stage:        repb.ExecutionStage_EXECUTING,
		ActionDigest: digest,
	})

	// Should have 1 pending (executing)
	if count := s.GetPendingTaskCount(); count != 1 {
		t.Errorf("GetPendingTaskCount() = %d during executing, want 1", count)
	}

	// Complete the operation
	s.Complete(op.Name, &repb.ExecuteResponse{})

	// Should have 0 pending again
	if count := s.GetPendingTaskCount(); count != 0 {
		t.Errorf("GetPendingTaskCount() = %d after complete, want 0", count)
	}
}

func TestExecutingCountTracking(t *testing.T) {
	s := NewScheduler(100, "test-node")
	ctx := context.Background()

	digest := &repb.Digest{
		Hash:      "abc123",
		SizeBytes: 100,
	}

	// Create two operations
	op1, _ := s.Enqueue(ctx, digest, false)
	op2, _ := s.Enqueue(ctx, digest, false)

	// Dequeue both
	s.Dequeue(ctx)
	s.Dequeue(ctx)

	// Mark both as executing
	s.UpdateState(op1.Name, StateExecuting, &repb.ExecuteOperationMetadata{
		Stage:        repb.ExecutionStage_EXECUTING,
		ActionDigest: digest,
	})
	s.UpdateState(op2.Name, StateExecuting, &repb.ExecuteOperationMetadata{
		Stage:        repb.ExecutionStage_EXECUTING,
		ActionDigest: digest,
	})

	// Should count both as pending
	if count := s.GetPendingTaskCount(); count != 2 {
		t.Errorf("GetPendingTaskCount() = %d with 2 executing, want 2", count)
	}

	// Complete one
	s.Complete(op1.Name, &repb.ExecuteResponse{})

	// Should count only 1
	if count := s.GetPendingTaskCount(); count != 1 {
		t.Errorf("GetPendingTaskCount() = %d with 1 executing, want 1", count)
	}

	// Fail the other
	s.Fail(op2.Name, nil)

	// Should be 0
	if count := s.GetPendingTaskCount(); count != 0 {
		t.Errorf("GetPendingTaskCount() = %d after all done, want 0", count)
	}
}
