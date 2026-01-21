package cluster

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"log/slog"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/colinrgodsey/goREgo/pkg/config"
	"github.com/google/uuid"
	"github.com/hashicorp/memberlist"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxBroadcastInterval = 10 * time.Millisecond
	dnsRecheckInterval   = 30 * time.Second
)

// LoadProvider is an interface for getting the current load of the local node.
type LoadProvider interface {
	GetPendingTaskCount() int
}

// Manager manages the cluster mesh using memberlist.
type Manager struct {
	mu sync.RWMutex

	cfg          config.ClusterConfig
	grpcAddress  string
	concurrency  int
	loadProvider LoadProvider
	logger       *slog.Logger

	list       *memberlist.Memberlist
	localState *NodeState
	peers      map[string]*NodeState // NodeID -> NodeState

	broadcasts *memberlist.TransmitLimitedQueue
}

// NewManager creates a new cluster manager.
func NewManager(cfg config.ClusterConfig, grpcAddress string, concurrency int, loadProvider LoadProvider, logger *slog.Logger) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}

	nodeID := cfg.NodeID
	if nodeID == "" {
		hostname, err := os.Hostname()
		if err != nil {
			nodeID = uuid.New().String()
		} else {
			nodeID = hostname
		}
	}

	m := &Manager{
		cfg:          cfg,
		grpcAddress:  grpcAddress,
		concurrency:  concurrency,
		loadProvider: loadProvider,
		logger:       logger.With("component", "cluster"),
		localState: &NodeState{
			Name:           nodeID,
			GrpcAddress:    grpcAddress,
			MaxConcurrency: int32(concurrency),
			LastUpdated:    timestamppb.Now(),
		},
		peers: make(map[string]*NodeState),
	}

	return m, nil
}

// Start initializes the memberlist and joins the cluster.
func (m *Manager) Start(ctx context.Context) error {
	mlConfig := memberlist.DefaultLANConfig()
	mlConfig.Name = m.localState.Name
	mlConfig.BindPort = m.cfg.BindPort

	if m.cfg.AdvertiseAddr != "" {
		mlConfig.AdvertiseAddr = m.cfg.AdvertiseAddr
	}
	mlConfig.AdvertisePort = m.cfg.BindPort

	// Configure delegate
	mlConfig.Delegate = m
	mlConfig.Events = m

	// Reduce logging noise
	mlConfig.Logger = log.New(&logAdapter{logger: m.logger}, "", 0)

	list, err := memberlist.Create(mlConfig)
	if err != nil {
		return fmt.Errorf("failed to create memberlist: %w", err)
	}
	m.list = list

	m.broadcasts = &memberlist.TransmitLimitedQueue{
		NumNodes: func() int {
			return m.list.NumMembers()
		},
		RetransmitMult: 3,
	}

	// Join existing peers
	peers, err := m.discoverPeers(ctx)
	if err != nil {
		return fmt.Errorf("peer discovery failed: %w", err)
	}

	if len(peers) > 0 {
		n, err := m.list.Join(peers)
		if err != nil {
			m.logger.Warn("failed to join some peers", "error", err, "joined", n)
		} else {
			m.logger.Info("joined cluster", "peers", n)
		}
	}

	return nil
}

// Stop gracefully leaves the cluster.
func (m *Manager) Stop(timeout time.Duration) error {
	if m.list == nil {
		return nil
	}

	if err := m.list.Leave(timeout); err != nil {
		return fmt.Errorf("failed to leave cluster: %w", err)
	}

	return m.list.Shutdown()
}

// discoverPeers resolves peers based on the discovery mode.
func (m *Manager) discoverPeers(ctx context.Context) ([]string, error) {
	switch m.cfg.DiscoveryMode {
	case "dns":
		return m.discoverDNS(ctx)
	case "list":
		fallthrough
	default:
		return m.cfg.JoinPeers, nil
	}
}

// discoverDNS resolves the DNS service name to a list of peer addresses.
func (m *Manager) discoverDNS(ctx context.Context) ([]string, error) {
	if m.cfg.DNSServiceName == "" {
		return nil, fmt.Errorf("dns_service_name is required for DNS discovery")
	}

	var ips []net.IPAddr
	var err error

	// Retry up to 5 times (5 seconds) to handle transient DNS failures during startup
	for i := 0; i < 5; i++ {
		ips, err = net.DefaultResolver.LookupIPAddr(ctx, m.cfg.DNSServiceName)
		if err == nil {
			break
		}

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		m.logger.Warn("DNS lookup failed, retrying...", "service", m.cfg.DNSServiceName, "attempt", i+1, "error", err)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(1 * time.Second):
			continue
		}
	}

	if err != nil {
		return nil, fmt.Errorf("DNS lookup failed for %s after retries: %w", m.cfg.DNSServiceName, err)
	}

	var peers []string
	for _, ip := range ips {
		addr := fmt.Sprintf("%s:%d", ip.IP.String(), m.cfg.BindPort)
		peers = append(peers, addr)
	}

	m.logger.Debug("DNS discovery resolved", "service", m.cfg.DNSServiceName, "peers", peers)
	return peers, nil
}

// NodeID returns the local node's unique identifier.
func (m *Manager) NodeID() string {
	return m.localState.Name
}

// GetLocalState returns the current state of the local node.
func (m *Manager) GetLocalState() *NodeState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	state := proto.Clone(m.localState).(*NodeState)
	if m.loadProvider != nil {
		state.PendingTasks = int32(m.loadProvider.GetPendingTaskCount())
	}
	state.LastUpdated = timestamppb.Now()
	return state
}

// GetPeers returns all known peers (excluding self).
func (m *Manager) GetPeers() []*NodeState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	peers := make([]*NodeState, 0, len(m.peers))
	for _, p := range m.peers {
		peers = append(peers, proto.Clone(p).(*NodeState))
	}
	return peers
}

// SelectBestPeer returns the peer with the lowest load that has capacity.
// Returns nil if the local node is the best choice or no peers have capacity.
func (m *Manager) SelectBestPeer() *NodeState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	localLoad := 0
	if m.loadProvider != nil {
		localLoad = m.loadProvider.GetPendingTaskCount()
	}
	localCapacity := m.concurrency

	// If local has capacity, consider it
	localHasCapacity := localLoad < localCapacity

	// Find the best peer
	var bestPeer *NodeState
	bestLoad := localLoad
	bestCapacity := localCapacity

	for _, peer := range m.peers {
		peerHasCapacity := int(peer.PendingTasks) < int(peer.MaxConcurrency)
		if !peerHasCapacity {
			continue
		}

		// Compare load ratios: pendingTasks / maxConcurrency
		// Lower is better
		peerLoadRatio := float64(peer.PendingTasks) / float64(peer.MaxConcurrency)
		bestLoadRatio := float64(bestLoad) / float64(bestCapacity)

		if peerLoadRatio < bestLoadRatio {
			bestPeer = peer
			bestLoad = int(peer.PendingTasks)
			bestCapacity = int(peer.MaxConcurrency)
		}
	}

	// If local has capacity and is better or equal, prefer local
	if localHasCapacity && bestPeer == nil {
		return nil
	}

	return bestPeer
}

// GetPeerByNodeID returns a peer by its node ID.
func (m *Manager) GetPeerByNodeID(nodeID string) *NodeState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if peer, ok := m.peers[nodeID]; ok {
		return proto.Clone(peer).(*NodeState)
	}
	return nil
}

// BroadcastState triggers an immediate broadcast of local state.
func (m *Manager) BroadcastState() {
	if m.broadcasts == nil {
		return
	}

	state := m.GetLocalState()
	data, err := proto.Marshal(state)
	if err != nil {
		m.logger.Error("failed to marshal state", "error", err)
		return
	}

	m.broadcasts.QueueBroadcast(&stateBroadcast{data: data})
}

// Delegate interface implementation for memberlist.

// NodeMeta returns metadata to include in the memberlist node state.
func (m *Manager) NodeMeta(limit int) []byte {
	state := m.GetLocalState()
	data, err := proto.Marshal(state)
	if err != nil {
		m.logger.Error("failed to marshal node metadata", "error", err)
		return nil
	}
	if len(data) > limit {
		m.logger.Warn("node metadata exceeds limit", "size", len(data), "limit", limit)
		return nil
	}
	return data
}

// NotifyMsg handles incoming messages from other nodes.
func (m *Manager) NotifyMsg(msg []byte) {
	if len(msg) == 0 {
		return
	}

	var state NodeState
	if err := proto.Unmarshal(msg, &state); err != nil {
		m.logger.Info("failed to unmarshal message", "error", err)
		return
	}

	if state.Name == m.localState.Name {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.peers[state.Name]; ok {
		// If the new address is a bind address but we have a valid existing address,
		// preserve the existing host and update the port.
		if host, port, err := net.SplitHostPort(state.GrpcAddress); err == nil {
			if host == "" || host == "0.0.0.0" || host == "::" {
				if existingHost, _, err := net.SplitHostPort(existing.GrpcAddress); err == nil {
					state.GrpcAddress = net.JoinHostPort(existingHost, port)
				}
			}
		}
		// Since state is a new unmarshaled object, we can just replace existing value
		// BUT we might want to keep some non-transient local tracking if we had any.
		// For now we just replace.
		m.peers[state.Name] = &state
	} else {
		// New peer via broadcast (unlikely before Join, but possible)
		m.peers[state.Name] = &state
	}
}

// GetBroadcasts returns any queued broadcasts.
func (m *Manager) GetBroadcasts(overhead, limit int) [][]byte {
	if m.broadcasts == nil {
		return nil
	}
	return m.broadcasts.GetBroadcasts(overhead, limit)
}

// LocalState implements memberlist.Delegate.LocalState.
// Note: This method name collides with our LocalState() method, so we implement
// the Delegate interface with a dedicated internal method.
func (m *Manager) LocalState(join bool) []byte {
	return m.NodeMeta(512)
}

// MergeRemoteState merges state received from remote nodes.
func (m *Manager) MergeRemoteState(buf []byte, join bool) {
	m.NotifyMsg(buf)
}

// EventDelegate interface implementation for memberlist.

// NotifyJoin is called when a node joins the cluster.
func (m *Manager) NotifyJoin(node *memberlist.Node) {
	if node.Name == m.localState.Name {
		return
	}

	var state NodeState
	if err := proto.Unmarshal(node.Meta, &state); err != nil {
		m.logger.Info("failed to unmarshal join metadata", "node", node.Name, "error", err)
		state = NodeState{
			Name:        node.Name,
			GrpcAddress: fmt.Sprintf("%s:%d", node.Addr.String(), node.Port),
			LastUpdated: timestamppb.Now(),
		}
	}

	// If the gRPC address is a bind address (0.0.0.0, ::, or empty host),
	// replace it with the node's gossip IP address.
	if host, port, err := net.SplitHostPort(state.GrpcAddress); err == nil {
		if host == "" || host == "0.0.0.0" || host == "::" {
			state.GrpcAddress = net.JoinHostPort(node.Addr.String(), port)
		}
	}

	m.mu.Lock()
	m.peers[node.Name] = &state
	m.mu.Unlock()

	m.logger.Info("node joined cluster", "node", node.Name, "grpc_address", state.GrpcAddress)
}

// NotifyLeave is called when a node leaves the cluster.
func (m *Manager) NotifyLeave(node *memberlist.Node) {
	if node.Name == m.localState.Name {
		return
	}

	m.mu.Lock()
	delete(m.peers, node.Name)
	m.mu.Unlock()

	m.logger.Info("node left cluster", "node", node.Name)
}

// NotifyUpdate is called when a node's metadata is updated.
func (m *Manager) NotifyUpdate(node *memberlist.Node) {
	if node.Name == m.localState.Name {
		return
	}

	var state NodeState
	if err := proto.Unmarshal(node.Meta, &state); err != nil {
		m.logger.Info("failed to unmarshal update metadata", "node", node.Name, "error", err)
		return
	}

	// Apply the same fix as NotifyJoin
	if host, port, err := net.SplitHostPort(state.GrpcAddress); err == nil {
		if host == "" || host == "0.0.0.0" || host == "::" {
			state.GrpcAddress = net.JoinHostPort(node.Addr.String(), port)
		}
	}

	m.mu.Lock()
	// Update regardless of existence
	m.peers[node.Name] = &state
	m.mu.Unlock()
}

// stateBroadcast implements memberlist.Broadcast.
type stateBroadcast struct {
	data []byte
}

func (b *stateBroadcast) Invalidates(other memberlist.Broadcast) bool {
	// Each new state update invalidates the previous one
	return true
}

func (b *stateBroadcast) Message() []byte {
	return b.data
}

func (b *stateBroadcast) Finished() {}

// Run starts a background goroutine that periodically broadcasts state.
func (m *Manager) Run(ctx context.Context) error {
	period := m.cfg.BroadcastPeriod
	if period <= 0 {
		period = 500 * time.Millisecond
	}
	ticker := time.NewTicker(period)
	defer ticker.Stop()

	// Check load changes frequently to propagate updates fast when busy
	loadTicker := time.NewTicker(maxBroadcastInterval)
	defer loadTicker.Stop()

	// Periodic DNS recheck
	dnsTicker := time.NewTicker(dnsRecheckInterval)
	defer dnsTicker.Stop()

	var lastLoad int
	if m.loadProvider != nil {
		lastLoad = m.loadProvider.GetPendingTaskCount()
	}

	for {
		select {
		case <-ticker.C:
			m.BroadcastState()
		case <-loadTicker.C:
			if m.loadProvider != nil {
				currentLoad := m.loadProvider.GetPendingTaskCount()
				if currentLoad != lastLoad {
					lastLoad = currentLoad
					// If load changed and is non-zero, broadcast immediately (throttled by ticker)
					// This helps peers avoid overloading this node
					if currentLoad > 0 {
						m.BroadcastState()
					}
				}
			}
		case <-dnsTicker.C:
			if m.cfg.DiscoveryMode == "dns" {
				peers, err := m.discoverDNS(ctx)
				if err != nil {
					m.logger.Warn("periodic DNS discovery failed", "error", err)
					continue
				}

				// Filter out already known peers
				newPeers := m.filterKnownPeers(peers)

				if len(newPeers) > 0 {
					n, err := m.list.Join(newPeers)
					if err != nil {
						m.logger.Warn("failed to join new peers from DNS", "error", err, "joined", n)
					} else if n > 0 {
						m.logger.Info("joined new peers from DNS", "count", n, "peers", newPeers)
					}
				}
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// filterKnownPeers filters out peers that are already known memberlist members.
func (m *Manager) filterKnownPeers(candidates []string) []string {
	if m.list == nil {
		return candidates
	}

	known := make(map[string]struct{})
	for _, member := range m.list.Members() {
		// Construct address string matching discoverDNS format
		addr := net.JoinHostPort(member.Addr.String(), fmt.Sprintf("%d", member.Port))
		known[addr] = struct{}{}
	}

	var newPeers []string
	for _, candidate := range candidates {
		// Normalize candidate just in case (though discoverDNS uses JoinHostPort logic manually)
		// discoverDNS: fmt.Sprintf("%s:%d", ip.IP.String(), m.cfg.BindPort)
		// We trust simple string comparison here as both come from IP+Port
		if _, exists := known[candidate]; !exists {
			newPeers = append(newPeers, candidate)
		}
	}
	return newPeers
}

// Members returns the number of known cluster members (including self).
func (m *Manager) Members() int {
	if m.list == nil {
		return 1
	}
	return m.list.NumMembers()
}

// SetLoadProvider sets the load provider for the cluster manager.
// This should be called after the scheduler is created.
func (m *Manager) SetLoadProvider(lp LoadProvider) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadProvider = lp
}

// HealthyMembers returns nodes that are currently reachable.
func (m *Manager) HealthyMembers() []string {
	if m.list == nil {
		return []string{m.localState.Name}
	}

	members := m.list.Members()
	names := make([]string, 0, len(members))
	for _, member := range members {
		names = append(names, member.Name)
	}
	sort.Strings(names)
	return names
}

// logAdapter adapts memberlist logs to slog.
type logAdapter struct {
	logger *slog.Logger
}

func (l *logAdapter) Write(p []byte) (n int, err error) {
	msg := string(bytes.TrimSpace(p))

	level := slog.LevelInfo
	if strings.Contains(msg, "[DEBUG]") {
		level = slog.LevelDebug
		msg = strings.Replace(msg, "[DEBUG]", "", 1)
	} else if strings.Contains(msg, "[WARN]") {
		level = slog.LevelWarn
		msg = strings.Replace(msg, "[WARN]", "", 1)
	} else if strings.Contains(msg, "[ERR]") || strings.Contains(msg, "[ERROR]") {
		level = slog.LevelError
		msg = strings.Replace(msg, "[ERR]", "", 1)
		msg = strings.Replace(msg, "[ERROR]", "", 1)
	} else if strings.Contains(msg, "[INFO]") {
		level = slog.LevelInfo
		msg = strings.Replace(msg, "[INFO]", "", 1)
	}

	msg = strings.TrimSpace(msg)
	msg = strings.TrimPrefix(msg, "memberlist:")
	msg = strings.TrimSpace(msg)

	// Log with the correct level (slog will filter based on configured level)
	l.logger.Log(context.Background(), level, msg, "subcomponent", "memberlist")

	return len(p), nil
}
