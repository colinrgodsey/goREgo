# Phase 4: linux-sandbox Execution Isolation

## Overview
Add linux-sandbox integration to the execution worker for hermetic, isolated command execution with read-only input protection and network isolation.

## Goals
- Protect CAS integrity by preventing commands from modifying input files
- Provide network isolation to ensure hermetic builds
- Use Linux namespaces (mount, network, PID) for process isolation
- Maintain compatibility with existing hard-link optimization

## Design Decisions
- **Failure mode**: Fail the action if sandbox setup fails (no fallback to unsandboxed execution)
- **Directory structure**: Separate `inputs/` and `execroot/` directories
- **Binary**: Assumes linux-sandbox is already installed on the system

---

## Architecture

### Directory Layout

Before (Phase 3):
```
buildRoot/{operationID}/
  ├── input_file.txt       (hard-linked from CAS)
  └── output.o             (created by command)
```

After (Phase 4):
```
buildRoot/{operationID}/
  ├── inputs/              (read-only bind mount for sandbox)
  │   └── input_file.txt   (hard-linked from CAS)
  └── execroot/            (working directory, writable)
      └── {WorkingDirectory}/
          └── output.o     (created by command)
```

### Sandbox Command Construction

```
linux-sandbox \
  -W {execRoot}                    # Working directory (writable)
  -M {source} -m {target}          # Bind mount each input file
  -w {outputDir}                   # Writable output directories
  -N                               # Network isolation (if enabled)
  -H                               # Set hostname to 'localhost'
  -T {timeout_secs}                # Timeout
  -t {kill_delay}                  # Kill delay after timeout
  -- \
  {command args...}
```

### Isolation Features

| Feature | Flag | Purpose |
|---------|------|---------|
| Mount namespace | `-M/-m` | Bind mount inputs read-only |
| Working directory | `-W` | Set writable execution root |
| Network namespace | `-N` | Disable network access |
| PID namespace | (automatic) | Isolate process visibility |
| Hostname | `-H` | Set to 'localhost' |

---

## Implementation Phases

### Phase 1: Configuration
- Add `SandboxConfig` struct to `pkg/config/config.go`
- Fields: `Enabled`, `BinaryPath`, `NetworkIsolation`, `WritablePaths`, `KillDelay`, `Debug`
- Add defaults in `LoadConfig()`

### Phase 2: Sandbox Package
- Create `pkg/sandbox/sandbox.go`
- `New(cfg)` - validates binary exists, returns nil if disabled
- `WrapCommand(spec)` - constructs linux-sandbox command line
- `Enabled()` - returns true if sandboxing is active
- Create `pkg/sandbox/sandbox_test.go` with table-driven tests

### Phase 3: Worker Integration
- Update `WorkerPool` struct to include `*sandbox.Sandbox`
- Change `NewWorkerPool` to return `(*WorkerPool, error)`
- Restructure `execute()` for new directory layout
- Add helpers: `buildInputMounts()`, `createOutputDirectories()`, `materializeInputsForUnsandboxed()`
- Update `runCommand()` to wrap command with sandbox when enabled
- Update `uploadOutputs()` for new path structure

### Phase 4: main.go Update
- Handle error return from `NewWorkerPool`

### Phase 5: Testing
- Unit tests for sandbox package
- Integration tests (skip if linux-sandbox unavailable):
  - Basic sandboxed execution
  - Input protection (read-only verification)
  - Network isolation verification
  - Timeout handling

---

## Configuration

```yaml
execution:
  enabled: true
  concurrency: 0
  build_root: "/tmp/gorego/builds"
  queue_size: 1000
  sandbox:
    enabled: true
    binary_path: "/usr/bin/linux-sandbox"
    network_isolation: true
    writable_paths: []
    kill_delay: 5
    debug: false
```

## Files to Modify

| File | Changes |
|------|---------|
| `pkg/config/config.go` | Add `SandboxConfig` |
| `pkg/sandbox/sandbox.go` | New - sandbox wrapper |
| `pkg/sandbox/sandbox_test.go` | New - unit tests |
| `pkg/execution/worker.go` | Directory restructure, sandbox integration |
| `pkg/execution/sandbox_test.go` | New - integration tests |
| `cmd/gorego/main.go` | Handle worker pool error |

## References
- [Bazel Sandboxing Documentation](https://bazel.build/docs/sandboxing)
- [linux-sandbox source](https://github.com/bazelbuild/bazel/blob/master/src/main/tools/linux-sandbox.cc)
