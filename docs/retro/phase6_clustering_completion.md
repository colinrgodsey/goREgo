# Integration Notes: Phase 6 Completion - Clustering & Mesh

## Overview
Phase 6 of the goREgo project has been successfully completed. The system now supports distributed execution via a peer-to-peer gossip mesh using `hashicorp/memberlist`. Nodes automatically discover peers, share load state, and intelligently route tasks to the least-loaded node in the cluster.

## Key Features Implemented

### 1. Configuration (`pkg/config`)
Expanded `ClusterConfig` with full cluster settings:
- `enabled`: Toggle cluster mode
- `node_id`: Unique node identifier (auto-generated from hostname if empty)
- `bind_port`: Gossip protocol port (default: 7946)
- `advertise_addr`: Address to advertise to peers (auto-detect if empty)
- `discovery_mode`: "list" for static peers, "dns" for Kubernetes headless services
- `join_peers`: Static list of peer addresses
- `dns_service_name`: DNS hostname for dynamic discovery

### 2. Cluster Manager (`pkg/cluster`)
New package implementing the distributed mesh:

#### NodeState
```go
type NodeState struct {
    Name           string // Unique Node ID
    GRPCAddress    string // Host:Port for gRPC traffic
    PendingTasks   int    // Active + Queued tasks
    MaxConcurrency int    // Capacity
    Tag            string // Optional affinity tag (future use)
}
```

#### Manager
- **Memberlist Integration**: Wraps `hashicorp/memberlist` for SWIM-based gossip
- **Delegate Implementation**: Broadcasts node state as JSON metadata
- **Event Handling**: Tracks node joins, leaves, and metadata updates
- **Peer Discovery**: Supports DNS resolution for Kubernetes headless services
- **Load Balancer**: `SelectBestPeer()` returns node with lowest load ratio

### 3. Scheduler Enhancements (`pkg/scheduler`)
- **Node-Prefixed Operation IDs**: Format `nodeID:uuid` for cluster routing
- **Executing Count Tracking**: Tracks tasks currently in execution
- **Load Reporting**: `GetPendingTaskCount()` returns queue length + executing count
- **Operation Parsing**: `ParseOperationNodeID()` extracts node ID from operation name

### 4. Execution Service Updates (`pkg/server/execution.go`)

#### Execute Flow with Clustering
1. Check action cache (existing behavior)
2. If cluster enabled, call `SelectBestPeer()`
3. If peer returned (lower load than local), forward request via gRPC
4. Otherwise, enqueue locally

#### WaitExecution Routing
1. Parse operation name for node ID prefix
2. If operation belongs to different node, forward to that peer
3. Otherwise, handle locally

#### Connection Pooling
- Maintains gRPC client connections to peers
- Thread-safe connection map with lazy initialization

### 5. Main Integration (`cmd/gorego/main.go`)
- Cluster manager initialization before scheduler
- Load provider wiring after scheduler creation
- Graceful cluster leave on shutdown (5 second timeout)
- Error group integration for background state broadcasts

### 6. Helm Chart Updates (`charts/gorego/`)
- **`values.yaml`**: Added `config.cluster` section with auto-enable logic
- **`templates/headless-service.yaml`**: New headless service for DNS discovery
- **`templates/statefulset.yaml`**: Gossip ports, cluster config in ConfigMap
- **`templates/NOTES.txt`**: Updated with clustering status and instructions

## Testing

### Unit Tests (`pkg/cluster/cluster_test.go`)
- `TestNewManager`: Manager creation with defaults and custom node ID
- `TestSelectBestPeer`: Load balancing logic with various scenarios
- `TestNodeMetaSerialization`: JSON encoding/decoding of node state
- `TestGetPeerByNodeID`: Peer lookup by node ID
- `TestSetLoadProvider`: Dynamic load provider wiring

### Integration Test
- `TestTwoNodeCluster`: Spawns two memberlist nodes, verifies discovery and peer selection

### Scheduler Tests (`pkg/scheduler/scheduler_test.go`)
- `TestParseOperationNodeID`: Node ID extraction from operation names
- `TestEnqueue_WithNodeIDPrefix`: Operation ID prefixing
- `TestGetPendingTaskCount`: Queue + executing count accuracy
- `TestExecutingCountTracking`: State transition counting

## Configuration Example

```yaml
cluster:
  enabled: true
  bind_port: 7946
  discovery_mode: "dns"
  dns_service_name: "gorego-headless.default.svc.cluster.local"
```

Or with static peers:
```yaml
cluster:
  enabled: true
  bind_port: 7946
  discovery_mode: "list"
  join_peers:
    - "10.0.0.1:7946"
    - "10.0.0.2:7946"
```

## Architecture Notes

### Data Flow: Distributed Execute
1. Client calls `Execute` on Node A
2. Node A checks action cache
3. Node A checks local load vs cluster peers
4. If Node B has lower load ratio, Node A forwards request to Node B
5. Node B enqueues task locally
6. Node B streams operation updates back through Node A to client
7. Client receives responses transparently

### Data Flow: WaitExecution Routing
1. Client calls `WaitExecution` with operation `operations/node-b:uuid`
2. Node A parses node ID prefix (`node-b`)
3. Node A looks up Node B's gRPC address from peer map
4. Node A forwards request to Node B
5. Node B streams updates through Node A to client

### Load Balancing Algorithm
```
For each peer:
  ratio = PendingTasks / MaxConcurrency
  if peer has capacity (ratio < 1) and ratio < best_ratio:
    select peer

If local has capacity and no better peer: use local
If all nodes at capacity: use local (backpressure)
```

## Dependencies Added
- `github.com/hashicorp/memberlist v0.5.4`: Gossip protocol implementation

## Known Limitations & Concerns

### 1. Network Partitions (Split-Brain)
- **Risk**: During network partitions, nodes may diverge and tasks could be duplicated
- **Mitigation**: Execution is inherently idempotent (same action digest = same result). Clients may experience retries, but no correctness issues
- **Accepted**: For the current use case, eventual consistency is acceptable

### 2. Shared Backing Cache Requirement
- **Assumption**: All cluster nodes must share the same Tier 2 backing cache
- **Why**: When Node B executes a task forwarded from Node A, it needs access to input blobs
- **Enforcement**: The write-through strategy ensures all blobs are available remotely
- **Risk**: If nodes have different backing cache configurations, execution may fail with missing inputs

### 3. Connection Pool Management
- **Current State**: Connections to peers are created lazily and never cleaned up
- **Risk**: Long-running clusters with high peer churn may accumulate stale connections
- **Future Work**: Add connection health checks and cleanup on peer leave events

### 4. No Affinity/Stickiness for Related Tasks
- **Current Behavior**: Each task is independently routed to the lowest-loaded node
- **Impact**: Related tasks (same action, similar inputs) may execute on different nodes
- **Trade-off**: Simplicity vs. cache locality optimization
- **Future Work**: Consider affinity hints based on input root digest

### 5. Single Forward Hop
- **Design Choice**: Forwarding is single-hop (Node A -> Node B), never multi-hop
- **Rationale**: Simplifies debugging and avoids cascading latency
- **Edge Case**: If Node B becomes unavailable mid-forward, client sees error (no automatic re-routing)

### 6. gRPC Insecure Credentials for Peer Communication
- **Current State**: Peer-to-peer gRPC uses `insecure.NewCredentials()`
- **Risk**: Not suitable for untrusted networks
- **Future Work**: Add TLS support for inter-node communication

### 7. State Broadcast Interval
- **Current**: 5-second ticker for state broadcasts
- **Trade-off**: Faster updates = more network traffic, slower updates = stale load info
- **Tuning**: May need adjustment based on cluster size and task churn

## Deployment Considerations

### Helm Chart Updates
The Helm chart (`charts/gorego/`) has been updated with full clustering support:

#### values.yaml Additions
```yaml
config:
  cluster:
    enabled: null      # Auto-enabled when replicaCount > 1
    bind_port: 7946    # Gossip port
    discovery_mode: "dns"
    dns_service_name: ""  # Auto-generated from headless service
    join_peers: []
```

#### New Headless Service Template
Created `templates/headless-service.yaml`:
- `clusterIP: None` for proper DNS-based discovery
- `publishNotReadyAddresses: true` for discovery during pod startup
- Exposes gossip (7946 TCP/UDP) and gRPC (50051) ports
- Only created when clustering is enabled

#### StatefulSet Updates
- Uses headless service name for `serviceName` when clustering enabled
- Adds gossip container ports (TCP and UDP)
- ConfigMap auto-generates DNS service name: `{release}-gorego-headless.{namespace}.svc.cluster.local`

#### Auto-Enable Logic
Clustering is automatically enabled when `replicaCount > 1`, unless explicitly overridden via `config.cluster.enabled`.

### Helm Usage Examples

**Single node (no clustering):**
```bash
helm install gorego ./charts/gorego
```

**Cluster with 3 nodes:**
```bash
helm install gorego ./charts/gorego --set replicaCount=3
```

**Cluster with shared backing cache (recommended):**
```bash
helm install gorego ./charts/gorego \
  --set replicaCount=3 \
  --set config.backing_cache.target="grpcs://my-remote-cache:443"
```

**Explicit clustering on single replica (for testing):**
```bash
helm install gorego ./charts/gorego \
  --set replicaCount=1 \
  --set config.cluster.enabled=true
```

### Port Requirements
- **7946/tcp+udp**: Memberlist gossip (configurable via `bind_port`)
- **50051/tcp**: gRPC services (configurable via `listen_addr`)

### Health Checks
The existing gRPC health check continues to work. Cluster membership is independent of the health status.

## Next Steps (Future Phases)
- **Metrics**: Add Prometheus metrics for cluster state (peer count, forward rate, load distribution)
- **TLS**: Secure inter-node communication
- **Connection Management**: Health checks and cleanup for peer connections
- **Affinity Hints**: Route related tasks to the same node for better cache utilization
- **Admin API**: Cluster status endpoint for debugging
