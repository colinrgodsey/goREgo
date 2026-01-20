package cluster

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/colinrgodsey/goREgo/pkg/config"
)

// mockLoadProvider implements LoadProvider for testing.
type mockLoadProvider struct {
	count int
}

func (m *mockLoadProvider) GetPendingTaskCount() int {
	return m.count
}

func TestNewManager(t *testing.T) {
	tests := []struct {
		name        string
		cfg         config.ClusterConfig
		grpcAddr    string
		concurrency int
		wantErr     bool
	}{
		{
			name: "creates manager with defaults",
			cfg: config.ClusterConfig{
				Enabled:       true,
				BindPort:      7946,
				DiscoveryMode: "list",
			},
			grpcAddr:    "localhost:50051",
			concurrency: 4,
			wantErr:     false,
		},
		{
			name: "uses custom node ID",
			cfg: config.ClusterConfig{
				Enabled:       true,
				NodeID:        "custom-node-id",
				BindPort:      7946,
				DiscoveryMode: "list",
			},
			grpcAddr:    "localhost:50051",
			concurrency: 8,
			wantErr:     false,
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := NewManager(tt.cfg, tt.grpcAddr, tt.concurrency, nil, logger)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewManager() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}

			if tt.cfg.NodeID != "" && m.NodeID() != tt.cfg.NodeID {
				t.Errorf("NodeID() = %v, want %v", m.NodeID(), tt.cfg.NodeID)
			}

			state := m.GetLocalState()
			if state.GrpcAddress != tt.grpcAddr {
				t.Errorf("GetLocalState().GrpcAddress = %v, want %v", state.GrpcAddress, tt.grpcAddr)
			}
			if int(state.MaxConcurrency) != tt.concurrency {
				t.Errorf("GetLocalState().MaxConcurrency = %v, want %v", state.MaxConcurrency, tt.concurrency)
			}
		})
	}
}

func TestSelectBestPeer(t *testing.T) {
	tests := []struct {
		name      string
		localLoad int
		localCap  int
		peers     map[string]*NodeState
		wantPeer  string // empty means local is best
	}{
		{
			name:      "returns nil when local has capacity and no peers",
			localLoad: 0,
			localCap:  4,
			peers:     map[string]*NodeState{},
			wantPeer:  "",
		},
		{
			name:      "returns nil when local has lowest load",
			localLoad: 1,
			localCap:  4,
			peers: map[string]*NodeState{
				"peer-1": {
					Name:           "peer-1",
					PendingTasks:   2,
					MaxConcurrency: 4,
				},
			},
			wantPeer: "",
		},
		{
			name:      "returns peer with lower load",
			localLoad: 3,
			localCap:  4,
			peers: map[string]*NodeState{
				"peer-1": {
					Name:           "peer-1",
					PendingTasks:   1,
					MaxConcurrency: 4,
				},
			},
			wantPeer: "peer-1",
		},
		{
			name:      "returns peer with better ratio",
			localLoad: 2,
			localCap:  4, // ratio: 0.5
			peers: map[string]*NodeState{
				"peer-1": {
					Name:           "peer-1",
					PendingTasks:   2,
					MaxConcurrency: 8, // ratio: 0.25
				},
			},
			wantPeer: "peer-1",
		},
		{
			name:      "skips peers at capacity",
			localLoad: 3,
			localCap:  4,
			peers: map[string]*NodeState{
				"peer-full": {
					Name:           "peer-full",
					PendingTasks:   4,
					MaxConcurrency: 4, // at capacity
				},
				"peer-available": {
					Name:           "peer-available",
					PendingTasks:   1,
					MaxConcurrency: 4,
				},
			},
			wantPeer: "peer-available",
		},
		{
			name:      "returns nil when all peers are full",
			localLoad: 4,
			localCap:  4,
			peers: map[string]*NodeState{
				"peer-full": {
					Name:           "peer-full",
					PendingTasks:   4,
					MaxConcurrency: 4,
				},
			},
			wantPeer: "",
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loadProvider := &mockLoadProvider{count: tt.localLoad}

			m, err := NewManager(config.ClusterConfig{
				Enabled:       true,
				NodeID:        "local-node",
				BindPort:      7946,
				DiscoveryMode: "list",
			}, "localhost:50051", tt.localCap, loadProvider, logger)
			if err != nil {
				t.Fatalf("NewManager() error = %v", err)
			}

			// Set up peers
			m.mu.Lock()
			m.peers = tt.peers
			m.mu.Unlock()

			got := m.SelectBestPeer()

			if tt.wantPeer == "" {
				if got != nil {
					t.Errorf("SelectBestPeer() = %v, want nil", got.Name)
				}
			} else {
				if got == nil {
					t.Errorf("SelectBestPeer() = nil, want %v", tt.wantPeer)
				} else if got.Name != tt.wantPeer {
					t.Errorf("SelectBestPeer() = %v, want %v", got.Name, tt.wantPeer)
				}
			}
		})
	}
}

func TestNodeMetaSerialization(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	loadProvider := &mockLoadProvider{count: 5}

	m, err := NewManager(config.ClusterConfig{
		Enabled:       true,
		NodeID:        "test-node",
		BindPort:      7946,
		DiscoveryMode: "list",
	}, "localhost:50051", 8, loadProvider, logger)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	meta := m.NodeMeta(512)
	if len(meta) == 0 {
		t.Error("NodeMeta() returned empty")
	}

	// Simulate receiving the meta on another node
	m.NotifyMsg(meta)

	// Verify local state wasn't corrupted
	state := m.GetLocalState()
	if state.Name != "test-node" {
		t.Errorf("GetLocalState().Name = %v, want test-node", state.Name)
	}
	if state.PendingTasks != 5 {
		t.Errorf("GetLocalState().PendingTasks = %v, want 5", state.PendingTasks)
	}
}

func TestGetPeerByNodeID(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	m, err := NewManager(config.ClusterConfig{
		Enabled:       true,
		NodeID:        "local-node",
		BindPort:      7946,
		DiscoveryMode: "list",
	}, "localhost:50051", 4, nil, logger)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	// Add a peer
	m.mu.Lock()
	m.peers["peer-1"] = &NodeState{
		Name:        "peer-1",
		GrpcAddress: "10.0.0.1:50051",
	}
	m.mu.Unlock()

	tests := []struct {
		name     string
		nodeID   string
		wantNil  bool
		wantAddr string
	}{
		{
			name:     "finds existing peer",
			nodeID:   "peer-1",
			wantNil:  false,
			wantAddr: "10.0.0.1:50051",
		},
		{
			name:    "returns nil for unknown peer",
			nodeID:  "unknown-peer",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := m.GetPeerByNodeID(tt.nodeID)
			if tt.wantNil {
				if got != nil {
					t.Errorf("GetPeerByNodeID(%q) = %v, want nil", tt.nodeID, got)
				}
			} else {
				if got == nil {
					t.Errorf("GetPeerByNodeID(%q) = nil, want non-nil", tt.nodeID)
				} else if got.GrpcAddress != tt.wantAddr {
					t.Errorf("GetPeerByNodeID(%q).GrpcAddress = %q, want %q", tt.nodeID, got.GrpcAddress, tt.wantAddr)
				}
			}
		})
	}
}

func TestSetLoadProvider(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	m, err := NewManager(config.ClusterConfig{
		Enabled:       true,
		NodeID:        "test-node",
		BindPort:      7946,
		DiscoveryMode: "list",
	}, "localhost:50051", 4, nil, logger)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	// Initially no load provider
	state := m.GetLocalState()
	if state.PendingTasks != 0 {
		t.Errorf("GetLocalState().PendingTasks = %v before SetLoadProvider, want 0", state.PendingTasks)
	}

	// Set load provider
	m.SetLoadProvider(&mockLoadProvider{count: 10})

	state = m.GetLocalState()
	if state.PendingTasks != 10 {
		t.Errorf("GetLocalState().PendingTasks = %v after SetLoadProvider, want 10", state.PendingTasks)
	}
}

func TestTwoNodeCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Create first node
	m1, err := NewManager(config.ClusterConfig{
		Enabled:       true,
		NodeID:        "node-1",
		BindPort:      17946,
		DiscoveryMode: "list",
	}, "localhost:50051", 4, &mockLoadProvider{count: 1}, logger)
	if err != nil {
		t.Fatalf("NewManager(node-1) error = %v", err)
	}

	if err := m1.Start(ctx); err != nil {
		t.Fatalf("m1.Start() error = %v", err)
	}
	defer m1.Stop(time.Second)

	// Create second node and join the first
	m2, err := NewManager(config.ClusterConfig{
		Enabled:       true,
		NodeID:        "node-2",
		BindPort:      17947,
		DiscoveryMode: "list",
		JoinPeers:     []string{"127.0.0.1:17946"},
	}, "localhost:50052", 4, &mockLoadProvider{count: 2}, logger)
	if err != nil {
		t.Fatalf("NewManager(node-2) error = %v", err)
	}

	if err := m2.Start(ctx); err != nil {
		t.Fatalf("m2.Start() error = %v", err)
	}
	defer m2.Stop(time.Second)

	// Wait for cluster to stabilize
	time.Sleep(500 * time.Millisecond)

	// Verify both nodes see each other
	if m1.Members() != 2 {
		t.Errorf("m1.Members() = %d, want 2", m1.Members())
	}
	if m2.Members() != 2 {
		t.Errorf("m2.Members() = %d, want 2", m2.Members())
	}

	// Verify peer discovery
	peers1 := m1.GetPeers()
	if len(peers1) != 1 {
		t.Errorf("m1.GetPeers() = %d peers, want 1", len(peers1))
	}

	peers2 := m2.GetPeers()
	if len(peers2) != 1 {
		t.Errorf("m2.GetPeers() = %d peers, want 1", len(peers2))
	}

	// Test SelectBestPeer - node-1 has lower load, so node-2 should select node-1
	peer := m2.SelectBestPeer()
	if peer == nil {
		t.Error("m2.SelectBestPeer() returned nil")
	} else if peer.Name != "node-1" {
		t.Errorf("m2.SelectBestPeer().Name = %v, want node-1", peer.Name)
	}
}
