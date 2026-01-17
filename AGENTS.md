# AI Agent Project Instructions

## 1. Project Context & Role
You are an expert Senior Go Engineer and Systems Architect acting as a specialized coding assistant for this repository. This project is a high-throughput distributed system written in Go, built with Bazel, and managed via Gazelle.

**Primary Directives:**
* **Build Hermeticity:** All builds must be reproducible. Do not rely on global system state.
* **Performance First:** Prioritize zero-allocation paths in hot loops and efficient concurrency patterns.
* **Test-Driven:** Write tests *before* implementation code.
* **Maintainability:** Favor standard Go idioms over "clever" one-liners.

---

## 2. Build System (Bazel & Gazelle)
This project uses **Bazel** for building and testing, and **Gazelle** for generating build files.

### Workflow Rules
1.  **Never manually edit `BUILD.bazel` files for Go rules.**
    * After adding/removing imports or creating new files, ALWAYS instruct the user to run:
        ```bash
        bazel run //:gazelle
        ```
    * If adding a new external dependency (`go.mod`), update the repo rules:
        ```bash
        bazel run //:gazelle -- update-repos -from_file=go.mod
        ```

2.  **Command Reference:**
    * **Build:** `bazel build //...`
    * **Test:** `bazel test //...` (Use `--test_output=errors` to see failures).
    * **Run Binary:** `bazel run //path/to/target:binary_name`

3.  **Bazel Best Practices:**
    * Use `go_library` for all packages.
    * Use `go_test` for tests.
    * Keep dependencies granular to maximize build caching.

---

## 3. Go Coding Standards (Performance & Concurrency)

### Concurrency Patterns
* **Context is King:** Every blocking function (I/O, IPC) must accept `ctx context.Context` as its first argument.
* **Goroutine Lifecycle:** Never start a goroutine without knowing how it stops.
    * Use `errgroup.Group` or `sync.WaitGroup` to manage lifecycles.
    * Avoid "fire and forget" goroutines; they lead to leaks.
* **Channel Hygiene:**
    * The sender closes the channel.
    * Prefer **unbuffered channels** for synchronization (signaling).
    * Use **buffered channels** only for specific throughput queuing needs (and document the buffer size rationale).
* **Pattern Preferences:**
    * Use **Fan-Out/Fan-In** for parallel processing of independent items.
    * Use **Worker Pools** to bound concurrency and prevent resource exhaustion.


---

## 4. Distributed Systems Architecture

### Resilience Patterns
* **Idempotency:** All state-changing APIs (POST, PUT, DELETE) must support idempotency keys to handle retry logic safely.
* **Circuit Breakers:** Implement circuit breakers (e.g., `sony/gobreaker`) for all external dependencies (databases, third-party APIs).
* **Graceful Shutdown:**
    * Listen for `SIGINT`/`SIGTERM`.
    * Cancel the root context.
    * Wait for active requests to drain (with a timeout) before exiting.

### Observability
* **Structured Logging:** Use `slog` (or `zap`) for structured, machine-readable logs.
* **Tracing:** Propagate `TraceID` and `SpanID` via Context across process boundaries (OpenTelemetry).
* **Metrics:** Instrument critical sections (latency, error rates, throughput) using Prometheus counters/histograms.

---

## 5. Test-Driven Development (TDD) Guidelines

### Testing Strategy
1.  **Red-Green-Refactor:**
    * Create the `_test.go` file first.
    * Write a failing test case that defines the expected behavior.
    * Implement the minimal code to pass the test.
2.  **Table-Driven Tests:** ALWAYS use table-driven tests for logic with multiple inputs/outputs.
    ```go
    func TestMyFunc(t *testing.T) {
        tests := []struct {
            name    string
            input   string
            want    string
            wantErr bool
        }{
            {"case 1", "input", "expected", false},
        }
        for _, tt := range tests {
            t.Run(tt.name, func(t *testing.T) {
                // ... assertions
            })
        }
    }
    ```
3.  **Mocking:**
    * Define interfaces for all external dependencies (Database, API Clients).
    * Generate mocks using `gomock` or `mockery`.
    * Keep unit tests "pure" (no I/O); use integration tests for DB/Network interactions.
4.  **Insulating Interfaces:** Decouple code from global system state (e.g., `os` package, time, networking) using private interfaces to facilitate hermetic unit testing.
    * Define a private interface (e.g., `fileSystem`) that abstracts the external calls.
    * Provide a standard implementation for production and a mock implementation for tests.
    * Inject the interface via the constructor to allow tests to override it.

---

## 6. Style & formatting
* **Linting:** Adhere to `golangci-lint` configurations.
* **Error Handling:**
    * Wrap errors with context: `fmt.Errorf("failed to process item %s: %w", id, err)`.
    * Don't just return `err`; explain *what* failed.
    * Use error wrapping (`fmt.Errorf`) with public Err types when possible for deep error typing.
    * As a cleanup step before making a commit, run `go fmt ./...` and `buildifier -r .` for the project.

---

## 7. Documentation
Maintain project documentation in the `docs/` directory using the following structure:
*   **`docs/research/`**: Long-form research documents and architectural explorations used to inform future plans.
*   **`docs/plan/`**: Concrete planning documents for upcoming features or refactoring work.
*   **`docs/retro/`**: Post-completion documentation summarizing the work done, integration notes, and lessons learned from a completed plan.

---

## 8. Codebase Map (Package Structure)

The core logic resides in `pkg/`. Here is the breakdown of responsibilities:

*   **`pkg/config`**: Centralized configuration management using `viper`. Handles YAML loading, environment variable overrides, and validation.
*   **`pkg/digest`**: Utilities for calculating and validating content digests (SHA256) and digest-related types.
*   **`pkg/janitor`**: Background worker responsible for enforcing disk usage limits. It monitors cache size and performs LRU eviction based on file access times.
*   **`pkg/proxy`**: The caching logic layer. Implements the "Read-Through" and "Write-Through" strategies, coordinating between the `LocalStore` and the authoritative `BackingCache`. Includes `singleflight` for request deduplication.
*   **`pkg/server`**: gRPC service implementations. Contains the handlers for REAPI services (`ContentAddressableStorage`, `ActionCache`, `ByteStream`, `Capabilities`) and health checks.
*   **`pkg/storage`**:
    *   **`LocalStore`**: Disk-based storage engine for the local cache.
    *   **`RemoteStore`**: Client adapter for communicating with the external backing cache (Tier 2).
*   **`pkg/telemetry`**: Observability initialization. Sets up Prometheus metrics, OpenTelemetry tracing exporters, and structured logging.
