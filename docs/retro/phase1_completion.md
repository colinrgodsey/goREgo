# Integration Notes: Phase 1 Completion - Caching Proxy

## Overview
Phase 1 of the goREgo project has been successfully completed. The system is now a functional, buildable caching proxy capable of handling Content Addressable Storage (CAS) requests via gRPC.

## Key Features Implemented

### 1. Build System Architecture
- **Tooling:** Migrated to a robust Bazel build system using `rules_go` and `gazelle`.
- **Dependency Management:**
    - Solved complex dependency chain issues between `remote-apis` (Bazel module) and `remote-apis-sdks` (Go module).
    - Adopted a "Pure Bazel Project" approach:
        - `bazel_remote_apis` is consumed directly as a Bazel module (v2.11.0-rc2).
        - `remote-apis-sdks` is fetched via `go_repository` with custom `gazelle:resolve` directives to link against the Bazel-generated protobuf rules.
        - This ensures a hermetic build where Go code is generated on-demand from the authoritative `.proto` definitions.

### 2. Core Components
- **Library Structure:** The codebase follows Go standards with a `lib/` directory structure (refactored from `internal/`).
- **Configuration:**
    - Implemented a unified configuration system using `spf13/viper`.
    - Supports YAML files (`config.yaml`) and Environment Variable overrides (e.g., `GOREGO_LISTEN_ADDR`).
    - Added `force_update_atime` option for `noatime` filesystems.
- **Storage Layer (Tier 1):**
    - `LocalStore`: A disk-based Content Addressable Storage implementation.
    - Shards files into `data/cas/ab/cd/hash` to avoid directory contention.
    - Implements `os.Chtimes` logic for explicit LRU tracking when configured.
- **Proxy Layer (Tier 2):**
    - `ProxyStore`: Implements the "Read-Through" and "Write-Through" logic.
    - Uses `singleflight` to dedup concurrent fetches for the same blob.
    - Currently configured with a passthrough/local-only mode if no remote target is specified.
- **Janitor:**
    - Background worker that monitors disk usage.
    - Performs LRU eviction based on filesystem `atime` (Access Time).
- **Server:**
    - gRPC server implementing the REAPI `ContentAddressableStorage` service.
    - Wired up to the `ProxyStore`.

## Build & Run Instructions

### Building
To build the entire project (binary + libraries):
```bash
bazel build //...
```

### Running
To run the server with the default configuration:
```bash
bazel run //cmd/gorego
```

To run with a specific config file:
```bash
bazel run //cmd/gorego -- --config config.yaml
```

## Next Steps (Phase 2 Preview)
- Implement the gRPC Client for the Backing Cache (Tier 2) to enable true proxying.
- Implement the Action Cache (AC) service fully.
- Add Observability (Prometheus Metrics & OpenTelemetry Tracing).
- Integration testing with a real Bazel build client.
