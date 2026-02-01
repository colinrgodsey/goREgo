# Concurrency Audit Fixes - Retro

**Date**: 2026-01-29
**Author**: AI Assistant
**Reference**: docs/plan/concurrency-audit.md

## Overview

This document summarizes the actions taken to address the high-severity issues identified in the Concurrency Audit Report.

## High Severity Issues

### 1. Missing context in compressor goroutine (`pkg/storage/remote.go`)
*   **Status**: Fixed
*   **Verification**: Created `pkg/storage/remote_leak_test.go` which reproduced the goroutine leak when `Put` was cancelled but input reader remained open.
*   **Fix**: Added `defer pr.Close()` in `Put` method. This ensures that the pipe reader is closed when `Put` returns (e.g., on context cancellation), which unblocks the background compressor goroutine (blocked on `pw.Write`) with `io.ErrClosedPipe`.

### 2. No timeout on stream read (`pkg/storage/remote.go`)
*   **Status**: Fixed
*   **Verification**: Analysis showed that `Get` relied on caller closing the returned reader, but if `Recv` hung on network, the background goroutine would leak.
*   **Fix**: Modified `Get` to create a cancellable child context for the stream. Implemented `cancelCloser` which cancels this context when the returned `ReadCloser` is closed. This ensures the background `Recv` loop terminates if the user closes the reader, or if the parent context is cancelled.

### 3. No timeout on stream write (`pkg/storage/remote.go`)
*   **Status**: Verified / Fixed
*   **Analysis**: `Put` uses `stream.Write(ctx)`. If `ctx` is cancelled, `Send` returns error. The hanging issue was primarily due to blocking reads on the input `io.Reader`.
*   **Fix**: The fix for Issue #1 (closing pipe reader) also resolves the blocking read issue in the compression case. For uncompressed input, standard `io.Reader` blocking rules apply, but `Put` will return if `stream.Send` fails on context cancel.

### 4. Context leak in Bytestream Write (`pkg/server/bytestream.go`)
*   **Status**: False Positive / Low Risk
*   **Analysis**: The `Write` method uses a buffered channel `make(chan error, 1)` to receive the result from the background `Put` goroutine. If `Write` returns early (due to context cancellation), the background goroutine will eventually complete (or fail) and send to the channel. Since the channel is buffered, the send will not block, and the goroutine will exit.

### 5. Missing ctx check in forwardExecute (`pkg/server/execution.go`)
*   **Status**: False Positive
*   **Analysis**: `forwardExecute` uses `peerStream.Recv()`. The `peerStream` is created with a context derived from the client's context. If the client context is cancelled, `peerStream.Recv()` returns an error (context canceled) according to gRPC semantics. An explicit `ctx.Done()` check is redundant.

### 6. Goroutine may leak on error (`pkg/server/bytestream.go`)
*   **Status**: Fixed (via Issue #2 fix)
*   **Analysis**: This referred to the `Read` method's background compressor. The leak occurred if `io.Copy` blocked on reading from the store. The fix for Issue #2 (ensuring `RemoteStore.Get` returns a context-aware reader/closer) resolves this. When `Read` returns, it closes the pipe, which closes the `RemoteStore` reader, which cancels the underlying stream context, unblocking the read.

## Medium Severity Issues

### 7. Double-lock in Scheduler.Enqueue (`pkg/scheduler/scheduler.go`)
*   **Status**: Fixed
*   **Analysis**: The original code released the lock before sending to `taskQueue`, then re-acquired it to handle the "queue full" case. This was inefficient and introduced a race condition where an operation existed in the map but not in the queue.
*   **Fix**: Refactored `Enqueue` to hold the lock throughout the operation. Since the channel send uses a non-blocking `select`, holding the lock is safe and correct.

## Testing

*   Added `pkg/storage/remote_leak_test.go` to regress goroutine leaks in `RemoteStore`.
*   Ran existing tests in `pkg/storage` and `pkg/scheduler` to ensure no regressions.

## Conclusion

The critical concurrency issues involving goroutine leaks and hangs in the storage layer have been resolved. The scheduling logic has been optimized for atomicity.
