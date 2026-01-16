# Phase 1 Implementation Plan: The Caching Proxy

**Goal:** Implement the Content Addressable Storage (CAS) and Action Cache (AC) gRPC services to serve as a high-performance, tiered proxy for Bazel.

## 1. Core Components

### 1.1 Configuration & Entrypoint
- Use a YAML configuration file for primary settings.
- Implement default values for all parameters.
- Support environment variable overrides for every configuration field (e.g., `GOREGO_LISTEN_ADDR`).
- Implement server startup, signal handling, and graceful shutdown.
- **Decision:** Use `spf13/viper` for robust YAML/ENV configuration management.

...

### 1.5 The Tiered Proxy Logic (Read-Through/Write-Through)
- **Read:** Check Tier 1 -> Miss -> Singleflight Fetch from Tier 2 -> Stream to Client + Write to Tier 1.
- **Write:** Write to Tier 2 (authoritative) -> Success -> Async/Sync write to Tier 1.
- **Dependency:** Maintain a hard dependency on the backing cache (Tier 2) to ensure artifact consistency across the mesh. If Tier 2 is unreachable, writes must fail.

### 1.6 gRPC Server Implementation
- Implement `ContentAddressableStorage` (REAPI).
    - `FindMissingBlobs`: Check Tier 1, then Tier 2.
    - `BatchUpdateBlobs`: Write-through.
    - `GetTree`: Fetch and parse directory blobs (maybe later optimization, standard blob fetch for now).
- Implement `ActionCache` (REAPI).
    - `GetActionResult`: Read-through.
    - `UpdateActionResult`: Write-through.
- **Capabilities Service:** Advertise REAPI version support.

## 2. Dependencies
- **Protobufs:** `github.com/bazelbuild/remote-apis`.
- **Concurrency:** `golang.org/x/sync/singleflight` (or `errgroup`).
- **Observability:** `prometheus` metrics for hits/misses, latencies.

## 3. Implementation Steps
1.  **Scaffold:** Create `internal/storage`, `internal/server`, `internal/config`.
2.  **Protos:** Import REAPI protos.
3.  **Local Store:** Implement the disk-based blob store with sharding.
4.  **Janitor:** Implement the disk usage monitor and cleanup.
5.  **Proxy Store:** Implement the composite store with `singleflight`.
6.  **gRPC API:** Wire up the REAPI handlers to the Proxy Store.
7.  **Integration Test:** Run Bazel against the local server.

## 4. Observations & Feedback
- **Protocol Version:** We must target REAPI v2 (standard).
- **Error Handling:** If Tier 2 is down, the node should probably fail writes (to preserve consistency) but could potentially serve reads from Tier 1. The research dictates a "Hard dependency," so we will fail hard on Tier 2 errors for now.
- **Validation:** Use `bazel-remote` or a mocked gRPC server as the Tier 2 backing store for testing.
