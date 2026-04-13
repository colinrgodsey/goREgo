package execution

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"

	wp "github.com/colinrgodsey/goREgo/pkg/execution/worker_protocol"
	"google.golang.org/protobuf/proto"
)

// WorkResult holds the result of executing a single work unit on a persistent
// worker process.
type WorkResult struct {
	ExitCode     int
	Output       string
	RequestId    int32
	WasCancelled bool
}

// ExecuteOption configures optional behavior for an Execute call.
type ExecuteOption func(*executeConfig)

type executeConfig struct {
	cancel bool
}

// WithCancel sends a cancel request for a previously sent WorkRequest.
func WithCancel(cancel bool) ExecuteOption {
	return func(cfg *executeConfig) {
		cfg.cancel = cancel
	}
}

// managedWorker holds the state for a single persistent worker process.
type managedWorker struct {
	key     string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	mu      sync.Mutex // serializes writes/reads to this worker
	restart func() *exec.Cmd
}

// PersistentWorkerManager manages a pool of long-lived worker processes keyed
// by identity. Workers that speak the Bazel persistent worker protocol
// (4-byte LE length-prefixed protobuf on stdin/stdout) can be reused across
// multiple work requests without the startup overhead of spawning a new
// process each time.
//
// The manager is safe for concurrent use. Workers are reused when the same
// workerKey is provided. If a worker process dies, it is automatically
// restarted and the work request is retried once.
type PersistentWorkerManager struct {
	mu      sync.Mutex
	workers map[string]*managedWorker
	restart map[string]func() *exec.Cmd
	logger  *slog.Logger
}

// NewPersistentWorkerManager creates a new PersistentWorkerManager.
func NewPersistentWorkerManager(logger *slog.Logger) *PersistentWorkerManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &PersistentWorkerManager{
		workers: make(map[string]*managedWorker),
		restart: make(map[string]func() *exec.Cmd),
		logger:  logger.With("component", "persistent_worker_manager"),
	}
}

// SetRestartCommand sets the command factory used to restart a worker when it
// dies unexpectedly. If not set, a dead worker cannot be restarted and Execute
// will return an error.
func (m *PersistentWorkerManager) SetRestartCommand(key string, cmd *exec.Cmd) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Store the path and args so we can create new Cmd instances on restart
	m.restart[key] = func() *exec.Cmd {
		return &exec.Cmd{
			Path: cmd.Path,
			Args: cmd.Args,
			Env:  cmd.Env,
			Dir:  cmd.Dir,
		}
	}
}

// Execute sends a work request to a persistent worker identified by workerKey.
// If no worker exists for the key, one is started using the provided cmd.
// If the worker is already running, the request is sent to the existing process.
// If the worker has died, it is restarted once and the request retried.
//
// Parameters:
//   - ctx: context for cancellation
//   - workerKey: unique key identifying the worker (same key = same process)
//   - cmd: the exec.Cmd to start a new worker (nil if worker already exists)
//   - arguments: per-request command-line arguments sent in WorkRequest
//   - inputs: per-request input files sent in WorkRequest
//   - opts: optional execution options (e.g., WithCancel)
func (m *PersistentWorkerManager) Execute(ctx context.Context, workerKey string, cmd *exec.Cmd, arguments []string, inputs []*wp.Input, opts ...ExecuteOption) (*WorkResult, error) {
	cfg := &executeConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	worker, err := m.getOrCreateWorker(ctx, workerKey, cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to get/create worker %q: %w", workerKey, err)
	}

	result, err := m.sendRequest(ctx, worker, &wp.WorkRequest{
		Arguments: arguments,
		Inputs:    inputs,
		Cancel:    cfg.cancel,
	})
	if err != nil {
		// Worker may have died. Try restarting once.
		m.logger.Debug("worker request failed, attempting restart", "key", workerKey, "error", err)

		worker, restartErr := m.restartWorker(ctx, workerKey)
		if restartErr != nil {
			return nil, fmt.Errorf("worker %q died and restart failed: original error: %w, restart error: %v", workerKey, err, restartErr)
		}

		result, err = m.sendRequest(ctx, worker, &wp.WorkRequest{
			Arguments: arguments,
			Inputs:    inputs,
			Cancel:    cfg.cancel,
		})
		if err != nil {
			return nil, fmt.Errorf("worker %q failed after restart: %w", workerKey, err)
		}
	}

	return result, nil
}

// Shutdown terminates all managed worker processes and cleans up resources.
func (m *PersistentWorkerManager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, w := range m.workers {
		m.logger.Debug("shutting down worker", "key", key)
		w.stdin.Close()
		w.stdout.Close()
		if w.cmd.Process != nil {
			_ = w.cmd.Process.Kill()
			_ = w.cmd.Wait()
		}
		delete(m.workers, key)
	}
}

// getOrCreateWorker returns an existing worker for the key, or starts a new one.
func (m *PersistentWorkerManager) getOrCreateWorker(ctx context.Context, workerKey string, cmd *exec.Cmd) (*managedWorker, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if w, ok := m.workers[workerKey]; ok {
		// Check if the process is still alive
		if w.cmd.Process != nil {
			if w.cmd.ProcessState == nil || !w.cmd.ProcessState.Exited() {
				return w, nil
			}
		}
		// Process is dead, clean it up
		w.stdin.Close()
		w.stdout.Close()
		delete(m.workers, workerKey)
	}

	if cmd == nil {
		return nil, fmt.Errorf("no worker exists for key %q and no command provided", workerKey)
	}

	worker, err := m.startWorker(workerKey, cmd)
	if err != nil {
		return nil, err
	}

	m.workers[workerKey] = worker
	return worker, nil
}

// restartWorker kills the existing worker (if any) and starts a new one using
// the stored restart command factory.
func (m *PersistentWorkerManager) restartWorker(ctx context.Context, workerKey string) (*managedWorker, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Clean up old worker
	if w, ok := m.workers[workerKey]; ok {
		w.stdin.Close()
		w.stdout.Close()
		if w.cmd.Process != nil {
			_ = w.cmd.Process.Kill()
			_ = w.cmd.Wait()
		}
		delete(m.workers, workerKey)
	}

	// Get restart command
	restartFn, ok := m.restart[workerKey]
	if !ok {
		return nil, fmt.Errorf("no restart command registered for key %q", workerKey)
	}

	cmd := restartFn()
	if cmd == nil {
		return nil, fmt.Errorf("restart command factory returned nil for key %q", workerKey)
	}

	worker, err := m.startWorker(workerKey, cmd)
	if err != nil {
		return nil, err
	}

	m.workers[workerKey] = worker
	return worker, nil
}

// startWorker starts a new worker process and returns a managedWorker.
// Caller must hold m.mu.
func (m *PersistentWorkerManager) startWorker(key string, cmd *exec.Cmd) (*managedWorker, error) {
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		stdinPipe.Close()
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	// Capture stderr for debugging but don't block
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		stdinPipe.Close()
		stdoutPipe.Close()
		return nil, fmt.Errorf("failed to start worker process: %w", err)
	}

	m.logger.Debug("started persistent worker", "key", key, "pid", cmd.Process.Pid)

	// Store the initial cmd as the restart factory if none registered
	if _, ok := m.restart[key]; !ok {
		path := cmd.Path
		args := make([]string, len(cmd.Args))
		copy(args, cmd.Args)
		env := make([]string, len(cmd.Env))
		copy(env, cmd.Env)
		dir := cmd.Dir
		m.restart[key] = func() *exec.Cmd {
			return &exec.Cmd{
				Path: path,
				Args: args,
				Env:  env,
				Dir:  dir,
			}
		}
	}

	return &managedWorker{
		key:    key,
		cmd:    cmd,
		stdin:  stdinPipe,
		stdout: stdoutPipe,
	}, nil
}

// sendRequest writes a WorkRequest and reads a WorkResponse from the worker.
// The worker's mu mutex is held for the duration to serialize concurrent access.
func (m *PersistentWorkerManager) sendRequest(ctx context.Context, w *managedWorker, req *wp.WorkRequest) (*WorkResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Write request
	if err := writeProto(w.stdin, req); err != nil {
		return nil, fmt.Errorf("failed to write request: %w", err)
	}

	// Read response with context awareness
	type readResult struct {
		resp *wp.WorkResponse
		err  error
	}
	ch := make(chan readResult, 1)

	go func() {
		resp, err := readProto(w.stdout)
		ch <- readResult{resp: resp, err: err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("failed to read response: %w", r.err)
		}
		return &WorkResult{
			ExitCode:     int(r.resp.ExitCode),
			Output:       r.resp.Output,
			RequestId:    r.resp.RequestId,
			WasCancelled: r.resp.WasCancelled,
		}, nil
	}
}

// writeProto writes a length-prefixed protobuf message to the writer.
// Wire format: 4-byte little-endian length + protobuf-encoded message.
func writeProto(w io.Writer, msg proto.Message) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}

	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(data)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// readProto reads a length-prefixed protobuf WorkResponse from the reader.
// Wire format: 4-byte little-endian length + protobuf-encoded message.
func readProto(r io.Reader) (*wp.WorkResponse, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	size := binary.LittleEndian.Uint32(lenBuf[:])

	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}

	var resp wp.WorkResponse
	if err := proto.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
