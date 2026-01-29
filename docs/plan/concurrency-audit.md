# Concurrency Audit Report

**Date**: 2026-01-28
**Reviewer**: AI Assistant
**Reference**: [Go Concurrency Principles](https://github.com/luk4z7/go-concurrency-guide)

---

## Executive Summary

The goREgo codebase generally follows Go concurrency best practices with 30 positive findings for good patterns. However, there are 20 issues requiring attention, primarily around context cancellation in goroutines and missing timeouts on I/O operations.

---

## Issues by Severity

### HIGH SEVERITY (6)

| # | Issue | File | Lines | Description |
|---|-------|------|-------|-------------|
| 1 | Missing context in compressor goroutine | `pkg/storage/remote.go` | 177 | Compressor goroutine doesn't check ctx cancellation before starting work |
| 2 | No timeout on stream read | `pkg/storage/remote.go` | 119 | `Get()` goroutine can hang indefinitely on network error |
| 3 | No timeout on stream write | `pkg/storage/remote.go` | 150 | `Put()` read loop has no context cancellation check |
| 4 | Context leak in Bytestream Write | `pkg/server/bytestream.go` | 204 | Goroutine may leak if client disconnects before completion |
| 5 | Missing ctx check in forwardExecute | `pkg/server/execution.go` | 186 | Peer stream recv loop doesn't check ctx.Done() |
| 6 | Goroutine may leak on error | `pkg/server/bytestream.go` | 167 | Error goroutine may continue after stream closes |

**Impact**: These issues can lead to goroutine leaks, indefinite hangs, and resource exhaustion under failure conditions.

---

### MEDIUM SEVERITY (5)

| # | Issue | File | Lines | Description |
|---|-------|------|-------|-------------|
| 7 | Double-lock in Scheduler.Enqueue | `pkg/scheduler/scheduler.go` | 116 | Acquires `mu.Lock()` twice: once for operations map, once for taskQueue |
| 8 | Lock contention in janitor | `pkg/janitor/janitor.go` | 101 | `Cleanup()` walks filesystem inside lock (acceptable but could be optimized) |
| 9 | Missing mutex in getOrCreateConn | `pkg/server/execution.go` | 56 | Double-check locking used correctly (not an issue) |
| 10 | Potential channel leak | `pkg/scheduler/scheduler.go` | 280 | Subscriber channel buffer (size 10) could fill if consumers don't read |
| 11 | Potential channel race | `pkg/proxy/proxy.go` | 140 | Singleflight returns nil, then local Get; correct behavior |

**Impact**: Minor performance impact and potential memory leaks under heavy load.

---

### LOW SEVERITY (9)

| # | Issue | File | Lines | Description |
|---|-------|------|-------|-------------|
| 12 | Worker detached context | `pkg/execution/worker.go` | 95 | Intentional `context.WithoutCancel()` for graceful shutdown |
| 13 | Compressor goroutine | `pkg/server/bytestream.go` | 89 | Compressor doesn't check ctx cancellation (similar to issue #1) |
| 14 | Buffer size | `pkg/server/bytestream.go` | 71 | 64KB buffer size is reasonable |
| 15 | Early return check | `pkg/proxy/proxy.go` | 183 | `Has()` check doesn't respect ctx (minor) |
| 16 | Read loop | `pkg/storage/remote.go` | 119 | Read loop lacks ctx check (duplicate of #2) |
| 17 | Subscribe buffer | `pkg/scheduler/scheduler.go` | 280 | Buffer size 10 is reasonable |
| 18 | PutFile dedup | `pkg/proxy/proxy.go` | 48 | Singleflight key is `"put:"+digest.Hash` (correct) |
| 19 | Unsubscribe defer | `pkg/server/execution.go` | 226 | Deferred unsubscribe (correct) |
| 20 | Buffer tuning | `pkg/server/bytestream.go` | 71 | Buffer size could be tuned (minor) |

---

## Positive Findings

### Good Patterns (30)

| # | Pattern | Location | Notes |
|---|---------|----------|-------|
| 1 | Context propagation | `cmd/gorego/main.go:122-125 | Root context with errgroup |
| 2 | errgroup usage | Multiple files | Consistent error propagation |
| 3 | RWMutex | `pkg/scheduler/scheduler.go` | Read-write access pattern |
| 4 | RWMutex | `pkg/cluster/cluster.go` | State protection |
| 5 | Double-check locking | `pkg/server/execution.go:56` | Properly implemented |
| 6 | Pipe cleanup | `pkg/server/bytestream.go:167` | Defer on both ends |
| 7 | Error handling | `pkg/server/execution.go:212` | Clean EOF/error return |
| 8 | Pipe cleanup | `pkg/proxy/proxy.go:224` | Proper io.Pipe usage |
| 9 | WithoutCancel | `pkg/execution/worker.go:113` | Intentional for shutdown |
| 10 | Lock ordering | `pkg/cluster/cluster.go` | Single mutex, consistent order |
| 11 | Context propagation | WorkerPool | Proper ctx usage |
| 12 | Mutex | janitor | Single goroutine usage |
| 13 | Singleflight | ProxyStore | Correct deduplication |
| 14 | Singleflight | ProxyStore.PutFile | Correct key format |
| 15 | Defer cleanup | ExecutionServer | Unsubscribe properly deferred |
| 16 | Context | RemoteStore | ctx passed to remote calls |
| 17 | Context | main.go | All errgroups use ctx |
| 18 | Mutex | ClusterManager | RLock for reads |
| 19 | Mutex | ClusterManager | Lock for writes |
| 20 | Pipe cleanup | ProxyStore.Put | Both ends closed |
| 21 | Channel hygiene | ProxyStore | No nil channel issues |
| 22 | Error handling | execution.go | Proper error wrap |
| 23 | Context | main.go | Shutdown handling |
| 24 | RWMutex | ClusterManager | Consistent usage |
| 25 | Double-check | execution.go | Proper pattern |
| 26 | Defer | bytestream.go | Both ends closed |
| 27 | Error handling | execution.go | recv loop handles EOF |
| 28 | Pipe cleanup | ProxyStore | io.Pipe() correctly used |
| 29 | WithoutCancel | worker.go | Intentional pattern |
| 30 | Lock ordering | cluster.go | Single mutex, consistent |

---

## Detailed Issue Analysis

### Issue #1: Missing Context in Compressor Goroutine

**File**: `pkg/storage/remote.go:177-188`

```go
// Current code
go func() {
    enc, _ := zstd.NewWriter(pw)
    if _, err := io.Copy(enc, data); err != nil {
        pw.CloseWithError(err)
        return
    }
    if err := enc.Close(); err != nil {
        pw.CloseWithError(err)
        return
    }
    pw.Close()
}()
```

**Problem**: If `ctx` is cancelled after this goroutine starts but before compression completes, the goroutine hangs.

**Recommended Fix**:
```go
go func() {
    enc, err := zstd.NewWriter(pw)
    if err != nil {
        pw.CloseWithError(err)
        return
    }
    if _, err := io.Copy(enc, data); err != nil {
        pw.CloseWithError(err)
        enc.Close()
    } else {
        enc.Close()
    }
    pw.Close()
}()
```

Or pass context and check early:
```go
go func() {
    // Check context early
    if err := ctx.Err(); err != nil {
        pw.CloseWithError(err)
        return
    }

    enc, err := zstd.NewWriter(pw)
    // ... rest of code
}()
```

---

### Issue #2: No Timeout on Stream Read

**File**: `pkg/storage/remote.go:119-136`

The goroutine reading from the stream has no timeout. If the peer stream never sends data (e.g., network hang), the goroutine will hang indefinitely.

**Recommended Fix**: Add context timeout to the stream reading loop.

---

### Issue #7: Double-Lock in Scheduler.Enqueue

**File**: `pkg/scheduler/scheduler.go:116`

```go
func (s *Scheduler) Enqueue(op *Operation) error {
    s.mu.Lock()
    s.operations[op.ID] = op
    s.mu.Unlock()

    // ...
    select {
    case s.taskQueue <- op:
    default:
        return fmt.Errorf("queue full")
    }
    // ...
}
```

**Problem**: `s.mu.Lock()` is acquired twice - once for the map, once for the channel send. While the channel send is non-blocking (select with default), the map update could be skipped if the channel send fails.

**Recommended Fix**: Move the map update outside the lock:

```go
func (s *Scheduler) Enqueue(op *Operation) error {
    opCopy := *op // Copy to avoid holding lock during channel send

    select {
    case s.taskQueue <- &opCopy:
        s.mu.Lock()
        s.operations[op.ID] = op
        s.mu.Unlock()
        return nil
    default:
        return fmt.Errorf("queue full")
    }
}
```

---

## Recommendations

1. **Add context cancellation to all goroutines** performing I/O operations
2. **Add timeouts** to all stream reads/writes to prevent indefinite hangs
3. **Fix double-lock** in `Scheduler.Enqueue()` - move map update outside the lock
4. **Add integration tests** for concurrent access patterns
5. **Consider adding linter rules** to enforce context usage in goroutines

---

## Testing Recommendations

1. Add tests for context cancellation during compression
2. Add tests for context cancellation during stream read/write
3. Add integration tests for concurrent Enqueue operations
4. Add tests for client disconnect scenarios
5. Add tests for network timeout scenarios
