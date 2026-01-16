# **Homogeneous Distributed Architecture for Bazel Remote Execution**

## **1\. Executive Summary**

This document presents the final architectural design for a high-performance, homogeneous Remote Execution (RE) service implemented in Go. The system is designed to replace legacy Java-based implementations (e.g., Buildfarm) with a single, unified binary that drastically simplifies operational complexity while improving scalability.

Core Philosophy:  
The architecture rejects the traditional separation of "Schedulers," "Workers," and "Storage Nodes." Instead, it employs a "Cell" model where every node is identical and capable of performing all roles. The cluster forms a peer-to-peer mesh using HashiCorp’s memberlist for coordination, relying on a mandatory external backing cache (e.g., S3-proxy, GCS, or a centralized NFS/gRPC store) as the authoritative source of truth.

## **2\. System Architecture**

### **2.1 The Node Role: "Proxy-Worker"**

Every node in the cluster runs the goREgo binary, which provides two primary functions simultaneously:

1. **Caching Proxy:** Intercepts CAS/AC requests. It serves hot artifacts from a local ephemeral NVMe/SSD cache and transparently fetches misses from the global backing cache.  
2. **Execution Worker:** Accepts build actions. It executes them locally if capacity allows or forwards them to a less-loaded peer in the mesh.

### **2.2 Global Data Flow**

1. **Submission:** A Bazel client sends a gRPC request (Execute, FindMissingBlobs, etc.) to *any* node in the cluster (via a simple Layer 4 Load Balancer or DNS Round Robin).  
2. **Resolution:**  
   * **Storage:** The receiving node satisfies the request using its local filesystem or fetches it from the Backing Cache.  
   * **Execution:** The receiving node checks its **Pending Task Count**. If it is below capacity, it executes the task. If full, it forwards the request to the peer with the lowest pending task count.

## **3\. Storage Architecture: The Tiered Proxy**

The storage layer is designed as a high-throughput **Write-Through / Read-Through Proxy**. The local disk is treated strictly as a performance buffer (LRU cache) and is not responsible for long-term data durability.

### **3.1 Tier 1: Local Filesystem Cache**

* **Directory Structure:** To avoid inode contention in a single directory, files are stored using a 2-level sharded structure based on the SHA-256 digest: data/cas/{ab}/{cd}/{abcdef...}.  
* **Metadata:** We rely on the filesystem's native atime (Access Time) to track usage. No external database (SQL/Redis) is required for index management.  
* **Concurrency:** We employ the **Singleflight** pattern. If 100 requests arrive for the same missing blob simultaneously, the node triggers only *one* fetch to the Backing Cache, streaming the result to all 100 clients while writing to disk.

### **3.2 Tier 2: Authoritative Backing Cache**

* **Requirement:** A persistent gRPC-compatible storage backend (e.g., a simplified bazel-remote instance, storage bucket proxy, or NFS mount).  
* **Behavior:**  
  * **Writes:** All writes (CAS blobs and Action Results) are uploaded to the Backing Cache. The client receives a success response only after the data is safely persisted in Tier 2\.  
  * **Reads:** On a local miss, the node fetches from Tier 2\. If Tier 2 returns 404, the data is definitively missing.

### **3.3 Garbage Collection: The "Casual Janitor"**

To maintain the local cache within disk limits without complex locking:

1. **High Watermark Trigger:** A background ticker monitors disk usage. If usage \> 90%:  
2. **Walk & Sort:** The Janitor walks the CAS directory, collecting file paths and atime stats.  
3. **Eviction:** It sorts files by atime (oldest first) and deletes them until usage drops to the Low Watermark (e.g., 75%).  
4. **Safety:** Files currently locked for reading/writing (tracked via an in-memory sync.Map of active operations) are skipped.

## **4\. Scheduling Architecture: The Mesh**

Scheduling relies on decentralized coordination via memberlist. There is no central queue; the "queue" is the aggregate capacity of the mesh.

### **4.1 Peer Discovery & State Sharing**

Nodes join a gossip mesh using memberlist. Every node broadcasts a lightweight state vector periodically (e.g., every 500ms \- 1s):

Go

type NodeState struct {  
    GrpcAddress      string  
    PendingTaskCount uint32 // Number of executing \+ queued actions  
    StateTimestamp   int64  // For conflict resolution  
}

Each node maintains a local, eventually consistent lookup table of all peers and their PendingTaskCount.

### **4.2 Load-Aware Routing Logic**

When a node receives an Execute request:

1. **Local Capacity Check:**  
   * if Local.PendingTaskCount \< Local.MaxConcurrency: Accept the task. Increment local counter. Execute.  
2. **Forwarding (Backpressure):**  
   * if Local.PendingTaskCount \>= Local.MaxConcurrency:  
     * Query the mesh table for the peer with the **lowest** PendingTaskCount.  
     * If Peer.PendingTaskCount \< Peer.MaxConcurrency, proxy the gRPC stream to that peer.  
     * *Optimization:* If the entire cluster is saturated (all nodes \> limit), the node queues the task locally in an in-memory buffer, applying standard backpressure (gRPC ResourceExhausted) to the client if the buffer fills up.

## **5\. Execution Engine: Direct & Ephemeral**

The system assumes it is deployed within a capable execution environment (e.g., a Kubernetes Pod or Docker container) that already provides the necessary build tools (compilers, linkers) and resource limits.

### **5.1 The Execution Lifecycle**

1. **Workspace Prep:**  
   * Create a temporary directory: /tmp/builds/{action\_id}.  
   * **Materialization:** Populate the input tree.  
     * *Fast Path:* If input files exist in the Local Cache, hardlink them into the workspace. This is near-instantaneous and consumes no extra disk space.  
     * *Slow Path:* If missing, download from Backing Cache \-\> Local Cache \-\> Hardlink.  
2. **Execution:**  
   * Use os/exec to spawn the command defined in the Action.  
   * Apply Action environment variables.  
   * Stream stdout/stderr to a temporary buffer.  
3. **Output Handling:**  
   * Upon success, scan the expected output files.  
   * Upload outputs to the Backing Cache (mandatory).  
   * Populate the Local Cache with these new outputs (for future hits).  
   * Update the Action Cache (AC) in the Backing Cache.  
4. **Cleanup:**  
   * rm \-rf /tmp/builds/{action\_id}.

## **6\. Implementation Roadmap**

### **Phase 1: The Caching Proxy (Storage)**

* **Goal:** Replace the current Java CAS with a Go binary that simply proxies to the backing store.  
* **Tasks:**  
  * Implement ContentAddressableStorage and ActionCache gRPC services.  
  * Implement the Singleflight read-through logic.  
  * Implement the "Janitor" for disk cleanup.  
* **Validation:** Point Bazel at this service using \--remote\_cache. Verify hits/misses and disk usage.

### **Phase 2: The Mesh (Discovery)**

* **Goal:** Enable nodes to see each other and share load data.  
* **Tasks:**  
  * Integrate hashicorp/memberlist.  
  * Create the Delegate to broadcast PendingTaskCount.  
  * Expose a debug HTTP endpoint /peers to visualize the cluster state.

### **Phase 3: The Scheduler & Executor**

* **Goal:** Handle Execute requests.  
* **Tasks:**  
  * Implement the Execute gRPC endpoint.  
  * Implement the routing logic: if busy { forward() } else { exec() }.  
  * Implement the os/exec wrapper and input tree materialization (hardlinking).  
* **Validation:** Run a full Bazel build with \--remote\_executor. Verify tasks are distributed across nodes even if the client only talks to one entry node.

## **7\. Configuration Summary**

A minimal configuration for the goREgo binary:

YAML

listen\_addr: ":50051"  
local\_cache\_dir: "/var/lib/gobazel/cache"  
local\_cache\_max\_size\_gb: 100  
max\_concurrent\_actions: 32

backing\_cache:  
  target: "s3-proxy.internal:9092"  
  \# or "legacy-buildfarm.internal:8980"

cluster:  
  join\_peers: \["10.0.0.5", "10.0.0.6"\] \# Seed nodes  
  gossip\_port: 7946

## **8\. Conclusion**

This architecture satisfies all constraints:

1. **Performance:** Go's concurrency model \+ NVMe local caching \+ Hardlinking inputs.  
2. **Scalability:** Memberlist mesh allows linear scaling; no central Redis bottleneck.  
3. **Simplicity:** Homogeneous nodes (single binary) reduce operational burden.  
4. **Robustness:** Hard dependency on the backing cache ensures no data loss if a worker node crashes.