# Phase 4 Implementation Plan: Clustering & Mesh

**Goal:** Transform the single-node `gorego` server into a distributed, load-aware cluster using a peer-to-peer mesh. This fulfills the "Cell" architecture vision where nodes dynamically share execution load.

## 1. Objectives
- **Discovery:** Nodes automatically discover peers and form a cluster.
- **Observability:** Nodes share their current load state (`PendingTaskCount`).
- **Routing:** The `Execution` service intelligently routes tasks to the least-loaded node.
- **Resilience:** The cluster survives node additions and removals without manual reconfiguration.

## 2. Architecture: The Mesh

### 2.1 Technology Choice
- **Library:** `hashicorp/memberlist` (Gossip protocol, SWIM-based).
- **Why:** Decentralized, failure detection, eventual consistency, proven in Consul/Nomad.

### 2.2 Node State (The Delegate)
Each node broadcasts a specialized message over the gossip layer:

```go
type NodeState struct {
    Name             string // Unique Node ID
    GrpcAddress      string // Host:Port for gRPC traffic
    PendingTasks     int    // Active + Queued tasks
    MaxConcurrency   int    // Capacity
    Tag              string // e.g., "cpu-high-perf" (future proofing)
}
```

### 2.3 The Cluster Manager
A new component `pkg/cluster` that:
1.  Wraps `memberlist`.
2.  Maintains a local lookup table: `map[NodeID]NodeState`.
3.  Exposes a `SelectBestPeer()` method.

## 3. Distributed Scheduling Logic

### 3.1 Modified `Execute` Flow
The `Execution` service in `Phase 3` simply enqueued locally. In `Phase 4`, we wrap this:

1.  **Receive Request:** Client calls `Execute`.
2.  **Check Local Load:** Is `Local.PendingTasks < Local.MaxConcurrency`?
    - **Yes:** Enqueue locally (Phase 3 logic).
    - **No:** Consult `ClusterManager`.
3.  **Forwarding (Proxying):**
    - `SelectBestPeer()` returns a `Peer` (lowest load, reachable).
    - If `Peer == Local` (everyone is full): Enqueue locally (backpressure buffer).
    - If `Peer != Local`:
        - Create a gRPC client connection to `Peer.GrpcAddress`.
        - Proxy the `Execute` stream to the peer.
        - Return the peer's responses to the client transparently.

### 3.2 Sticky Operations
- **Problem:** `WaitExecution` might be called on Node A for an operation running on Node B.
- **Solution A (Simple):** Clients usually maintain the stream. If the stream breaks, they call `WaitExecution`.
    - We need to know *where* the operation lives.
    - **Approach:** Encode the NodeID in the `OperationID` (e.g., `node-01:uuid`).
    - When `WaitExecution` is called, check the prefix. If not local, proxy to the correct node.

## 4. Configuration Changes
Update `config.yaml`:

```yaml
cluster:
  enabled: true
  bind_port: 7946 # Gossip port
  bootstrap_peers: ["10.0.0.1:7946", "10.0.0.2:7946"]
  advertise_addr: "10.0.0.5" # Optional, auto-detect if empty
```

## 5. Implementation Steps

1.  **Cluster Package:** Implement `pkg/cluster` with `memberlist`.
2.  **State Broadcasting:** Implement the `Delegate` interface to share `PendingTasks`.
3.  **Load Balancer:** Implement `SelectBestPeer` logic.
4.  **Operation Naming:** Update `TaskScheduler` to mint Operation IDs with the local Node ID prefix.
5.  **Proxy Logic:**
    - Update `Execution.Execute` to forward requests.
    - Update `Execution.WaitExecution` to forward based on ID prefix.
6.  **Integration Test:**
    - Spawn 2 `gorego` processes locally (ports 50051/7946 and 50052/7947).
    - Send 100 tasks to Node A.
    - Verify Node B receives ~50 tasks (assuming concurrency limits trigger forwarding).

## 6. Dependencies
- `github.com/hashicorp/memberlist`

## 7. Risks & Unknowns
- **Network Partitions:** Split-brain is possible.
    - *Mitigation:* Execution is stateless (idempotent). If a partition happens, the client might retry. We accept this for now.
- **CAS Consistency:** This architecture assumes a shared Backing Cache (Tier 2). If nodes don't share Tier 2, Node B can't run Node A's action if inputs are missing.
    - *Verification:* The design strictly enforces "Write-Through" to Tier 2, so this should be safe.
