# Phase 2 Implementation Plan: Observability & Tooling

**Goal:** Implement comprehensive observability (Metrics, Tracing, Structured Logging) to make the system production-ready and debuggable.

## 1. Objectives
- **Visibility:** Real-time dashboarding of cache hit rates, latency, and throughput.
- **Debugging:** Distributed tracing to pinpoint bottlenecks (e.g., is the slow request due to disk I/O, remote fetch, or network?).
- **Reliability:** Standard health checks for orchestration (Kubernetes).

## 2. Architecture

### 2.1 Metrics (Prometheus)
We will use the standard Prometheus Go client to expose an HTTP `/metrics` endpoint.

*   **Library:** `github.com/prometheus/client_golang`
*   **Endpoint:** Expose a separate HTTP server (e.g., port `:9090` or configured via `telemetry.metrics_addr`).
*   **Key Metrics:**
    *   `gorego_cache_requests_total{method, tier, status}`: Counter for hits/misses/errors.
    *   `gorego_cache_latency_seconds{method, tier}`: Histogram for operation duration.
    *   `gorego_blob_size_bytes{method}`: Histogram for payload sizes.
    *   `gorego_disk_usage_bytes`: Gauge for current local cache usage (updated by Janitor).
    *   `gorego_janitor_evictions_total`: Counter for files/bytes evicted.
    *   **Go Runtime:** Standard Go metrics (GC, goroutines, memory).

### 2.2 Tracing (OpenTelemetry)
We will integrate OpenTelemetry (OTel) to trace requests across the system boundaries (Client -> Proxy -> Local/Remote).

*   **Library:** `go.opentelemetry.io/otel` (and associated exporters).
*   **Exporter:** OTLP (gRPC) to support generic backends (Jaeger, Tempo, Honeycomb).
*   **Propagation:** Use `b3` or `w3c` trace context propagation to maintain trace IDs from incoming requests to outgoing remote fetches.
*   **Instrumentation Points:**
    *   **gRPC Interceptors:** Automatically trace all incoming and outgoing gRPC calls.
    *   **Store Layer:** Manually create spans for `LocalStore.Get`, `RemoteStore.Put`, etc., to differentiate between "Queueing/Locking" and "IO".

### 2.3 Structured Logging
*   **Library:** `log/slog` (Standard Library).
*   **Integration:** Ensure Logger attaches `trace_id` and `span_id` from the context to log entries automatically.

### 2.4 Health Checks
*   **Standard:** Implement `grpc.health.v1.Health` service.
*   **Probes:**
    *   `Liveness`: Returns OK if the binary is running.
    *   `Readiness`: Returns OK if `LocalStore` is writable and (optionally) `BackingCache` is reachable.

## 3. Configuration Changes
Update `config.yaml` and `Config` struct:

```yaml
telemetry:
  metrics_addr: ":9090"
  tracing_endpoint: "localhost:4317" # OTLP gRPC
  log_level: "info" # debug, info, warn, error
```

## 4. Implementation Steps

1.  **Dependencies:** Add Prometheus and OTel libraries to `go.mod` and `MODULE.bazel`.
2.  **Telemetry Package:** Create `pkg/telemetry` to handle setup/shutdown of providers.
3.  **Metrics:**
    *   Initialize Prometheus registry.
    *   Start HTTP server for `/metrics`.
    *   Add `prometheus.InstrumentHandler` or gRPC interceptors.
4.  **Tracing:**
    *   Configure OTLP exporter.
    *   Configure global TracerProvider.
    *   Add `go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc` interceptors.
5.  **Manual Instrumentation:**
    *   Add spans to `ProxyStore` logic (e.g., "deduplicating request").
    *   Instrument `Janitor` runs.
6.  **Verification:**
    *   Run local Jaeger/Prometheus (using Docker/Compose separately, or manual setup).
    *   Verify traces appear for `GetBlob` requests.

## 5. Dependencies
*   `github.com/prometheus/client_golang`
*   `go.opentelemetry.io/otel`
*   `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc`
*   `go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc`
