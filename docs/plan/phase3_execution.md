# Phase 3 Implementation Plan: Single-Node Execution

**Goal:** Implement the `Execution` and `Operations` gRPC services to enable a single `gorego` node to accept and process build actions locally. This establishes the "Worker" role defined in the architecture, but restricts scope to a single node (no clustering yet).

## 1. Objectives
- **Execution Service:** Implement `Execute` and `WaitExecution` (REAPI v2).
- **Operations Service:** Implement `GetOperation`, `ListOperations`, `CancelOperation`, `DeleteOperation` (Long-Running Operations).
- **Local Worker:** Implement the "Direct & Ephemeral" execution engine using `os/exec`.
- **Validation:** Successfully run a Bazel build with `--remote_executor=grpc://localhost:50051`.

## 2. Architecture: The Local Executor

Since we are deferring clustering to Phase 4, the "Scheduler" in this phase is a simple in-memory queue managed by the local process.

### 2.1 Component: `TaskScheduler`
- **Responsibility:** Manages the lifecycle of an Operation.
- **State:** In-memory map of `OperationID -> OperationStatus`.
- **Queue:** Buffered channel or priority queue of `Action` requests waiting for a worker slot.
- **Backpressure:** If the queue is full, reject `Execute` requests with `RESOURCE_EXHAUSTED`.

### 2.2 Component: `LocalWorker`
- **Concurrency:** A fixed pool of goroutines (configurable via `execution.concurrency`, default: runtime.NumCPU()).
- **Lifecycle:**
    1.  **Dequeue:** Pick an Action from the `TaskScheduler`.
    2.  **Stage:** Create a temp directory (e.g., `/tmp/gorego/actions/<id>`).
        - Query CAS (local/proxy) for the `Action` and `Command` protos.
        - Materialize the input tree (use hardlinks/symlinks if possible for local-to-local optimization, but standard file copy is safer for MVP).
    3.  **Execute:**
        - Wrap `os/exec`.
        - Apply environment variables from `Command`.
        - Enforce timeouts (`Command.platform` properties or default).
        - Capture `stdout` and `stderr` (stream to memory/temp file).
    4.  **Upload:**
        - Scan output files/directories.
        - Upload outputs to CAS (via `LocalStore/ProxyStore`).
        - Upload `stdout`/`stderr` blobs.
    5.  **Finalize:**
        - Construct `ActionResult`.
        - Update Action Cache (AC).
        - Mark Operation as `Done`.
    6.  **Cleanup:** `rm -rf` the temp directory.

## 3. gRPC Service Implementation

### 3.1 `Execution` Service
- **`Execute`:**
    - Create a new Operation ID (UUID).
    - Store initial metadata.
    - Enqueue the task in `TaskScheduler`.
    - Stream back the initial `Operation` state (usually `QUEUED`).
- **`WaitExecution`:**
    - Subscribe to state changes for the given Operation ID.
    - Stream updates until `Done`.

### 3.2 `Operations` Service
- Standard implementation to query the status of in-flight or recently completed operations.
- **Retention:** Keep completed operations in memory for a short window (e.g., 5-10 minutes) to allow clients to poll final status.

### 3.3 Capabilities
- Update `GetCapabilities` to return `execution_capabilities` (REAPI v2).

## 4. Configuration Changes
Update `config.yaml`:

```yaml
execution:
  enabled: true
  concurrency: 0 # 0 = NumCPU
  build_root: "/tmp/gorego/builds"
```

## 5. Implementation Steps

1.  **Scaffold:** Create `pkg/execution`, `pkg/scheduler`.
2.  **TaskScheduler:** Implement the in-memory operation tracker and queue.
3.  **Worker Logic (Staging):** Implement input root materialization (fetching from CAS).
4.  **Worker Logic (Exec):** Implement `os/exec` wrapper with context cancellation and output capturing.
5.  **Worker Logic (Output):** Implement output scanning and CAS upload.
6.  **gRPC Wiring:** Register `Execution` and `Operations` servers.
7.  **Integration Test:**
    - Create a test that uploads a simple "echo hello" action to CAS.
    - Call `Execute`.
    - Verify `ActionResult` contains "hello".

## 6. Dependencies
- `github.com/google/uuid`: For Operation IDs.
- `google.golang.org/genproto/googleapis/longrunning`: For Operations proto types.

## 7. Risks & Unknowns
- **Sandbox Isolation:** We are using raw `os/exec` (process isolation only). This is not hermetic (can see `/tmp`, `/usr/bin`).
    - *Mitigation:* Accept this limitation for Phase 3. Real sandboxing (nsjail/docker) is a future enhancement.
- **Input Materialization Performance:** Copying files is slow.
    - *Mitigation:* Phase 3 MVP can copy. Future optimization: Hardlinking from the CAS cache (if on same filesystem).
