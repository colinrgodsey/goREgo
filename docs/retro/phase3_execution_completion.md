# Integration Notes: Phase 3 Completion - Single-Node Execution

## Overview
Phase 3 of the goREgo project has been successfully completed. The system now supports single-node execution via the REAPI Execution and Operations services.

## Key Features Implemented

### 1. Configuration
- Added `execution` config section:
  - `enabled`: Toggle execution support
  - `concurrency`: Worker pool size (default: NumCPU)
  - `build_root`: Directory for build sandboxes
  - `queue_size`: Max pending tasks

### 2. Task Scheduler (`pkg/scheduler`)
- **In-memory operation tracking**: Maps operation ID to status
- **Task queue**: Buffered channel with backpressure (returns `RESOURCE_EXHAUSTED` when full)
- **Subscription system**: Channels for streaming operation updates
- **State machine**: QUEUED -> EXECUTING -> COMPLETED
- **Retention policy**: Cleans up completed operations after 10 minutes

### 3. Local Worker (`pkg/execution`)
- **Worker pool**: Fixed-size goroutine pool (configurable concurrency)
- **Input staging**: Recursive directory materialization from CAS
- **Command execution**: `os/exec` wrapper with:
  - Context-based timeout
  - Environment variable injection
  - Working directory support
  - stdout/stderr capture
- **Output collection**:
  - Output files scanning and upload
  - Output directories with Tree proto construction
  - stdout/stderr blob upload
- **Cleanup**: Automatic build directory removal after execution

### 4. gRPC Services

#### Execution Service (`pkg/server/execution.go`)
- **Execute**: Enqueues action, checks action cache first (unless skip_cache_lookup), streams operation updates
- **WaitExecution**: Subscribes to existing operation and streams updates

#### Operations Service (`pkg/server/operations.go`)
- **GetOperation**: Returns current operation status
- **ListOperations**: Returns all tracked operations
- **CancelOperation**: Cancels queued operations
- **DeleteOperation**: Removes operation from tracker

### 5. Capabilities
- Updated `GetCapabilities` to return `execution_capabilities` with `ExecEnabled: true` when configured

## Testing
Added comprehensive integration tests (`pkg/execution/worker_test.go`):
- Basic echo command execution
- Output file capture
- Non-zero exit code handling
- Input materialization verification
- Build directory cleanup verification

## Build & Run Instructions

### Building
```bash
bazel build //...
```

### Running with Execution Enabled
Create a `config.yaml`:
```yaml
listen_addr: ":50051"
local_cache_dir: "/var/lib/gorego/cache"
execution:
  enabled: true
  concurrency: 4
  build_root: "/tmp/gorego/builds"
```

Then run:
```bash
bazel run //cmd/gorego -- --config config.yaml
```

### Testing with Bazel
```bash
bazel build //path/to/target \
  --remote_executor=grpc://localhost:50051 \
  --remote_cache=grpc://localhost:50051
```

## Architecture Notes

### Data Flow
1. Client calls `Execute` with action digest
2. Server checks action cache (optional)
3. If miss, task is enqueued
4. Worker dequeues task
5. Worker fetches Action/Command/InputRoot from CAS
6. Worker materializes input tree to temp directory
7. Worker executes command via `os/exec`
8. Worker uploads outputs (files, directories, stdout, stderr)
9. Worker constructs ActionResult
10. Worker updates action cache (if exit code 0)
11. Operation marked complete

### Limitations (Accepted for Phase 3)
- **No sandbox isolation**: Uses raw `os/exec`, not hermetic
- **Single-node only**: No distributed execution yet

## Dependencies Added
- `github.com/google/uuid`: Operation ID generation
- `cloud.google.com/go/longrunning`: Operations service protos

## Post-Release Debugging & Fixes

### Output Directory Creation & OutputPaths Support
During initial integration testing with Bazel, we encountered errors where executed commands failed because output directories (or parent directories of output files) did not exist.

#### Issue 1: Missing Parent Directories
Commands like `gcc` and `cp` expect target directories to exist. The worker was originally only staging inputs but not pre-creating output directories.
**Fix**: Added logic to `WorkerPool.execute` to iterate through `Command.OutputFiles` and `Command.OutputDirectories` and create all necessary parent directories using `os.MkdirAll`.

#### Issue 2: REAPI v2.1+ `OutputPaths`
After fixing the initial issue, we observed that `OutputFiles` and `OutputDirectories` were empty in the debug logs, yet commands still failed with "No such file or directory". This indicated the client was using the newer `OutputPaths` field (introduced in REAPI v2.1), which supersedes the separate file/directory fields.
**Fix**:
1.  **Execution Setup**: Updated `WorkerPool.execute` to also iterate through `Command.OutputPaths` and create parent directories for all entries.
2.  **Output Collection**: Updated `WorkerPool.uploadOutputs` to handle `OutputPaths`. Since `OutputPaths` does not distinguish between files and directories, the worker now dynamically `Stat`s each path after execution:
    -   If it's a directory, it's processed as an output directory (Tree proto construction).
    -   If it's a file, it's processed as an output file.
    -   If it doesn't exist, it's ignored (optional outputs).

### Input Materialization Optimization
To improve performance and reduce disk I/O, the input staging mechanism was optimized to use **hard links** (`os.Link`) instead of full file copies when the local CAS is available.
- **Mechanism**: The worker checks if the underlying blob store is a `LocalBlobStore`. If so, it attempts to hard-link the cached CAS blob to the execution sandbox.
- **Fallback**: If hard-linking fails (e.g., cross-device link), the worker gracefully falls back to copying the file.
- **Safety**: To support this, we ensure that executable bits are set correctly on the CAS blobs if required by the action, while maintaining the immutable nature of the content (hard links share the underlying inode).

### Output Collection Optimization
Similarly, output collection was optimized to hard-link generated files directly into the local CAS.
- **Mechanism**: Instead of reading the output file into memory and writing it to the store, the worker calculates the digest and then hard-links the file from the build directory to the CAS.
- **Remote Upload**: The `ProxyStore` ensures that even when the local write is optimized via hard-linking, the file is still concurrently streamed to the remote backing cache (if configured).

### ByteStream Write Size Mismatch & Resumable Uploads
During high-concurrency runs, we observed `digest size mismatch: expected X, got Y` errors in the `ByteStream` service.
- **Issue**: Some clients were attempting resumable uploads (sending `WriteOffset > 0`). Since the server currently only supports atomic `Put` operations starting from offset 0, it would treat the partial stream as the full blob, leading to a size mismatch against the digest in the resource name.
- **Fix**: Added strict validation in `ByteStream.Write`. The server now returns `InvalidArgument` if `WriteOffset > 0` is received, providing a clear error to the client instead of a confusing size mismatch.

## Next Steps (Phase 4)
- **Clustering**: Implement `hashicorp/memberlist` peer-to-peer mesh
- **Distributed Scheduling**: Load-aware task distribution
- **Sandboxing**: Consider nsjail/Docker for hermetic execution