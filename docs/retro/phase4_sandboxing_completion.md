# Integration Notes: Phase 4 Completion - Sandbox Execution Isolation

## Overview
Phase 4 of the goREgo project adds linux-sandbox integration to the execution worker for hermetic, isolated command execution. This protects CAS integrity by preventing commands from modifying input files and provides optional network isolation.

## Key Features Implemented

### 1. Hermetic `linux-sandbox` Build
To ensure portability and ease of setup, the `linux-sandbox` tool is now built from source as part of the project:
- **`bazel` Repository Import**: Added a module extension in `bazel_deps.bzl` to fetch the Bazel source code (v8.5.1).
- **Custom Patching**: Applied patches during the fetch to:
  - Stub out complex Protocol Buffer dependencies (`execution_statistics.proto`) with a dummy C++ header, allowing the tool to be built without a full Bazel-in-Bazel proto toolchain.
  - Inject a minimal `BUILD` file for the sandbox tool.
- **Local Alias**: Added an alias `//src/main/tools:linux-sandbox` in the project for easy access to the hermetic binary.
- **Worker Integration**: The default configuration can now point to this built-in binary, reducing external system dependencies.

### 2. Configuration
Added `sandbox` config section under `execution`:
- `enabled`: Toggle sandbox support (default: false)
- `binary_path`: Path to linux-sandbox binary (default: /usr/bin/linux-sandbox)
- `network_isolation`: Enable network namespace isolation (default: true)
- `writable_paths`: Additional paths to make writable (default: [])
- `kill_delay`: Seconds after timeout before SIGKILL (default: 5)
- `debug`: Enable sandbox debug output (default: false)

### 2. Sandbox Package (`pkg/sandbox`)
New package providing sandbox wrapper functionality:
- **`New(cfg)`**: Validates binary exists, returns nil if disabled
- **`WrapCommand(spec)`**: Constructs linux-sandbox command line with:
  - `-W` for working directory (writable)
  - `-M/-m` for bind mount pairs (read-only inputs)
  - `-w` for additional writable paths
  - `-N` for network isolation
  - `-H` for hostname
  - `-T/-t` for timeout handling
- **`Enabled()`**: Check if sandboxing is active

### 3. Directory Structure Redesign
Changed from single workDir to separated inputs/execroot:

**Before (Phase 3):**
```
buildRoot/{operationID}/
  ├── input_file.txt       (hard-linked from CAS)
  └── output.o             (created by command)
```

**After (Phase 4):**
```
buildRoot/{operationID}/
  ├── inputs/              (hard-linked from CAS, read-only bind mount)
  │   └── input_file.txt
  └── execroot/            (working directory, writable)
      └── {WorkingDirectory}/
          └── output.o
```

### 4. Worker Integration (`pkg/execution`)
- **WorkerPool struct**: Added `*sandbox.Sandbox` field
- **NewWorkerPool**: Now returns `(*WorkerPool, error)` to handle sandbox init failures
- **execute()**: Restructured for new directory layout
- **runCommand()**: Updated signature to accept inputMounts and writablePaths, wraps command with sandbox when enabled
- **New helper functions**:
  - `buildInputMounts()`: Walks inputs directory, builds source→target map
  - `createOutputDirectories()`: Creates output dirs, returns writable paths
  - `materializeInputsForUnsandboxed()`: Hard-links inputs when sandbox disabled
  - `deduplicatePaths()`: Removes duplicate writable paths

### 5. Sandbox Isolation Features
When enabled, linux-sandbox provides:
- **Mount namespace**: Bind mount inputs read-only into execroot
- **Network namespace**: Disable network access (optional via `-N`)
- **PID namespace**: Isolate process visibility
- **Hostname isolation**: Set to 'localhost' via `-H`

## Testing
Added comprehensive test coverage:

### Unit Tests (`pkg/sandbox/sandbox_test.go`)
- `TestNew_DisabledReturnsNil`: Verify nil sandbox when disabled
- `TestNew_EnabledBinaryNotFound`: Error handling for missing binary
- `TestSandbox_Enabled`: Check Enabled() behavior
- `TestWrapCommand`: Table-driven tests for command construction
- `TestWrapCommand_ArgumentOrder`: Verify correct argument ordering
- `TestDeduplicatePaths`: Path deduplication logic

### Integration Tests (`pkg/execution/sandbox_integration_test.go`)
Tests skip automatically if linux-sandbox is not available:
- `TestSandbox_BasicExecution`: Simple echo command in sandbox
- `TestSandbox_OutputFileCapture`: Output file creation and collection
- `TestSandbox_InputFileAccess`: Reading input files through bind mounts
- `TestSandbox_InputProtection`: Verify inputs cannot be modified
- `TestSandbox_NetworkIsolation`: Network access blocked with `-N`
- `TestSandbox_NonZeroExitCode`: Exit code propagation
- `TestSandbox_WorkingDirectory`: Working directory handling
- `TestSandbox_EnvironmentVariables`: Env var passthrough

## Build & Run Instructions

### Building
```bash
bazel build //...
```

### Running with Sandbox Enabled
Create a `config.yaml`:
```yaml
listen_addr: ":50051"
local_cache_dir: "/var/lib/gorego/cache"
execution:
  enabled: true
  concurrency: 4
  build_root: "/tmp/gorego/builds"
  sandbox:
    enabled: true
    binary_path: "/usr/bin/linux-sandbox"
    network_isolation: true
```

Then run:
```bash
bazel run //cmd/gorego -- --config config.yaml
```

### Prerequisites
- Linux with namespace support (kernel 3.8+)
- `linux-sandbox` binary (from Bazel installation or built separately)
- `CONFIG_USER_NS` kernel option enabled

## Architecture Notes

### Data Flow (Sandboxed)
1. Task dequeued from scheduler
2. Action/Command/InputRoot fetched from CAS
3. Create `inputs/` and `execroot/` directories
4. Stage inputs to `inputs/` via hard links from CAS
5. Build input mounts map (source → target)
6. Create output directories in `execroot/`
7. Wrap command with linux-sandbox:
   - Bind mount each input file read-only
   - Set execroot as writable working directory
   - Apply network isolation if configured
8. Execute wrapped command
9. Collect outputs from `execroot/` via hard links to CAS
10. Update action cache (if exit code 0)
11. Cleanup base directory

### Fallback Behavior (Unsandboxed)
When sandbox is disabled:
1. Inputs staged to `inputs/` directory
2. Inputs materialized into `execroot/` via hard links/symlinks
3. Command executed directly without isolation
4. Same output collection process

## Files Changed

| File | Changes |
|------|---------|
| `pkg/config/config.go` | Added `SandboxConfig` struct and defaults |
| `pkg/sandbox/sandbox.go` | New - sandbox wrapper implementation |
| `pkg/sandbox/sandbox_test.go` | New - unit tests |
| `pkg/execution/worker.go` | Directory restructure, sandbox integration |
| `pkg/execution/sandbox_integration_test.go` | New - integration tests |
| `pkg/execution/worker_test.go` | Updated for new constructor signature |
| `pkg/execution/hardlink_test.go` | Updated for new constructor signature |
| `cmd/gorego/main.go` | Handle worker pool error return |
| `bazel_deps.bzl` | New - module extension for Bazel source |
| `MODULE.bazel` | Import bazel source repository |
| `src/main/tools/BUILD.bazel` | New - alias for linux-sandbox |
| `AGENTS.md` | Added execution architecture documentation |
| `docs/plan/phase4_sandboxing.md` | Planning document |

## Dependencies
No new external dependencies. Uses standard library and existing project packages.

## Known Limitations
- Requires linux-sandbox binary to be pre-installed
- User namespaces must be enabled in kernel
- Some systems require `sysctl kernel.unprivileged_userns_clone=1`

## Next Steps (Phase 5)
- **Clustering**: Implement `hashicorp/memberlist` peer-to-peer mesh
- **Distributed Scheduling**: Load-aware task distribution across nodes
