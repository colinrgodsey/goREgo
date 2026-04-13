package execution

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	wp "github.com/colinrgodsey/goREgo/pkg/execution/worker_protocol"
	"google.golang.org/protobuf/proto"
)

// fakeWorkerEnv is the environment variable used to signal that the test binary
// should run as a fake persistent worker instead of running tests.
const fakeWorkerEnv = "GOREGO_TEST_FAKE_WORKER_MODE"

// fakeWorkerBehaviorEnv selects the behavior of the fake worker.
const fakeWorkerBehaviorEnv = "GOREGO_TEST_FAKE_WORKER_BEHAVIOR"

func init() {
	if mode := os.Getenv(fakeWorkerEnv); mode != "" {
		runFakeWorker(mode)
		os.Exit(0)
	}
}

// runFakeWorker implements a fake persistent worker that reads WorkRequests from
// stdin and writes WorkResponses to stdout using the Bazel wire protocol
// (4-byte LE length prefix + protobuf).
//
// Behavior modes:
//   - "echo": echoes arguments as output, exit code 0
//   - "fail": returns exit code 1 with "failed" output
//   - "crash_after": reads one request, responds, then exits (simulates crash)
//   - "slow": adds a 500ms delay before responding
//   - "invalid_response": sends back non-protobuf garbage
func runFakeWorker(behavior string) {
	stdin := os.Stdin
	stdout := os.Stdout
	crashOnReadCount := 0

	for {
		req, err := readWorkRequest(stdin)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			return
		}

		if req.Cancel {
			resp := &wp.WorkResponse{
				RequestId:    req.RequestId,
				WasCancelled: true,
			}
			_ = writeWorkResponse(stdout, resp)
			continue
		}

		resp := &wp.WorkResponse{
			RequestId: req.RequestId,
		}

		switch behavior {
		case "echo":
			resp.ExitCode = 0
			// Concatenate arguments into output
			output := ""
			for i, arg := range req.Arguments {
				if i > 0 {
					output += " "
				}
				output += arg
			}
			resp.Output = output

		case "fail":
			resp.ExitCode = 1
			resp.Output = "failed"

		case "crash_after":
			// Respond to first request, then exit
			resp.ExitCode = 0
			resp.Output = "crashing"
			_ = writeWorkResponse(stdout, resp)
			return // Simulate crash after first response

		case "crash_on_read":
			// Read request, respond to first one, then exit on second read
			if crashOnReadCount == 0 {
				crashOnReadCount++
				resp.ExitCode = 0
				resp.Output = "ok-before-crash"
				_ = writeWorkResponse(stdout, resp)
				continue
			}
			return // Crash on subsequent read without responding

		case "slow":
			time.Sleep(500 * time.Millisecond)
			resp.ExitCode = 0
			resp.Output = "slow"

		case "invalid_response":
			// Send garbage data
			var buf [4]byte
			binary.LittleEndian.PutUint32(buf[:], 5)
			_, _ = stdout.Write(buf[:])
			_, _ = stdout.Write([]byte("GARBAGE"))
			return

		default:
			resp.ExitCode = 0
			resp.Output = "ok"
		}

		if err := writeWorkResponse(stdout, resp); err != nil {
			return
		}
	}
}

// readWorkRequest reads a length-prefixed protobuf WorkRequest.
func readWorkRequest(r io.Reader) (*wp.WorkRequest, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	size := binary.LittleEndian.Uint32(lenBuf[:])

	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}

	var req wp.WorkRequest
	if err := proto.Unmarshal(data, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// writeWorkResponse writes a length-prefixed protobuf WorkResponse.
func writeWorkResponse(w io.Writer, resp *wp.WorkResponse) error {
	data, err := proto.Marshal(resp)
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

// fakeWorkerCmd returns an exec.Cmd that re-executes the current test binary
// as a fake worker with the given behavior.
func fakeWorkerCmd(t *testing.T, behavior string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		fakeWorkerEnv+"="+behavior,
		fakeWorkerBehaviorEnv+"="+behavior,
	)
	return cmd
}

func TestPersistentWorkerManager_Execute(t *testing.T) {
	tests := []struct {
		name       string
		behavior   string // fake worker behavior
		workerKey  string
		arguments  []string
		wantCode   int
		wantOutput string
		wantErr    bool
	}{
		{
			name:       "echo hello world",
			behavior:   "echo",
			workerKey:  "worker-echo",
			arguments:  []string{"hello", "world"},
			wantCode:   0,
			wantOutput: "hello world",
			wantErr:    false,
		},
		{
			name:       "failing worker",
			behavior:   "fail",
			workerKey:  "worker-fail",
			arguments:  []string{"do-something"},
			wantCode:   1,
			wantOutput: "failed",
			wantErr:    false,
		},
		{
			name:       "echo with single argument",
			behavior:   "echo",
			workerKey:  "worker-echo-single",
			arguments:  []string{"compile"},
			wantCode:   0,
			wantOutput: "compile",
			wantErr:    false,
		},
		{
			name:       "echo with no arguments",
			behavior:   "echo",
			workerKey:  "worker-echo-empty",
			arguments:  []string{},
			wantCode:   0,
			wantOutput: "",
			wantErr:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := NewPersistentWorkerManager(slog.Default())

			t.Cleanup(func() {
				mgr.Shutdown()
			})

			cmd := fakeWorkerCmd(t, tt.behavior)
			result, err := mgr.Execute(context.Background(), tt.workerKey, cmd, tt.arguments, nil)

			if (err != nil) != tt.wantErr {
				t.Fatalf("Execute() error = %v, wantErr %v", err, tt.wantErr)
			}

			if err != nil {
				return
			}

			if result.ExitCode != tt.wantCode {
				t.Errorf("ExitCode = %d, want %d", result.ExitCode, tt.wantCode)
			}
			if result.Output != tt.wantOutput {
				t.Errorf("Output = %q, want %q", result.Output, tt.wantOutput)
			}
		})
	}
}

func TestPersistentWorkerManager_WorkerReuse(t *testing.T) {
	mgr := NewPersistentWorkerManager(slog.Default())
	t.Cleanup(func() {
		mgr.Shutdown()
	})

	cmd1 := fakeWorkerCmd(t, "echo")
	result1, err := mgr.Execute(context.Background(), "shared-key", cmd1, []string{"first"}, nil)
	if err != nil {
		t.Fatalf("First Execute() failed: %v", err)
	}
	if result1.Output != "first" {
		t.Errorf("First output = %q, want %q", result1.Output, "first")
	}

	// Second request with the same key should reuse the same worker process.
	// We pass a different cmd but it should be ignored since the worker is already running.
	cmd2 := fakeWorkerCmd(t, "echo")
	result2, err := mgr.Execute(context.Background(), "shared-key", cmd2, []string{"second"}, nil)
	if err != nil {
		t.Fatalf("Second Execute() failed: %v", err)
	}
	if result2.Output != "second" {
		t.Errorf("Second output = %q, want %q", result2.Output, "second")
	}

	// Verify the manager only has one worker for this key
	mgr.mu.Lock()
	count := len(mgr.workers)
	mgr.mu.Unlock()
	if count != 1 {
		t.Errorf("Expected 1 worker, got %d", count)
	}
}

func TestPersistentWorkerManager_DifferentKeysDifferentWorkers(t *testing.T) {
	mgr := NewPersistentWorkerManager(slog.Default())
	t.Cleanup(func() {
		mgr.Shutdown()
	})

	cmd1 := fakeWorkerCmd(t, "echo")
	cmd2 := fakeWorkerCmd(t, "echo")

	result1, err := mgr.Execute(context.Background(), "key-A", cmd1, []string{"from-A"}, nil)
	if err != nil {
		t.Fatalf("Execute key-A failed: %v", err)
	}
	if result1.Output != "from-A" {
		t.Errorf("key-A output = %q, want %q", result1.Output, "from-A")
	}

	result2, err := mgr.Execute(context.Background(), "key-B", cmd2, []string{"from-B"}, nil)
	if err != nil {
		t.Fatalf("Execute key-B failed: %v", err)
	}
	if result2.Output != "from-B" {
		t.Errorf("key-B output = %q, want %q", result2.Output, "from-B")
	}

	mgr.mu.Lock()
	count := len(mgr.workers)
	mgr.mu.Unlock()
	if count != 2 {
		t.Errorf("Expected 2 workers, got %d", count)
	}
}

func TestPersistentWorkerManager_AutoRestart(t *testing.T) {
	mgr := NewPersistentWorkerManager(slog.Default())
	t.Cleanup(func() {
		mgr.Shutdown()
	})

	// Use "crash_after" behavior: worker responds to one request then exits
	cmd := fakeWorkerCmd(t, "crash_after")

	// First request succeeds
	result1, err := mgr.Execute(context.Background(), "crash-key", cmd, []string{"before-crash"}, nil)
	if err != nil {
		t.Fatalf("First Execute() failed: %v", err)
	}
	if result1.Output != "crashing" {
		t.Errorf("First output = %q, want %q", result1.Output, "crashing")
	}

	// Second request: worker has crashed, should auto-restart and succeed
	// Need to provide the command for the restart
	mgr.SetRestartCommand("crash-key", fakeWorkerCmd(t, "echo"))

	result2, err := mgr.Execute(context.Background(), "crash-key", nil, []string{"after-restart"}, nil)
	if err != nil {
		t.Fatalf("Second Execute() after restart failed: %v", err)
	}
	if result2.Output != "after-restart" {
		t.Errorf("Second output = %q, want %q", result2.Output, "after-restart")
	}
}

func TestPersistentWorkerManager_RestartFailure(t *testing.T) {
	mgr := NewPersistentWorkerManager(slog.Default())
	t.Cleanup(func() {
		mgr.Shutdown()
	})

	// Start a worker that responds once then crashes
	cmd := fakeWorkerCmd(t, "crash_after")

	// First request succeeds
	_, err := mgr.Execute(context.Background(), "restart-fail", cmd, []string{"first"}, nil)
	if err != nil {
		t.Fatalf("First Execute() failed: %v", err)
	}

	// Set restart to a command that doesn't exist (simulates restart failure)
	badCmd := exec.Command("/nonexistent/binary/that/does/not/exist")
	mgr.SetRestartCommand("restart-fail", badCmd)

	// Second request should fail because restart also fails
	_, err = mgr.Execute(context.Background(), "restart-fail", nil, []string{"second"}, nil)
	if err == nil {
		t.Error("Expected error when restart command fails, got nil")
	}
}

func TestPersistentWorkerManager_InvalidResponse(t *testing.T) {
	mgr := NewPersistentWorkerManager(slog.Default())
	t.Cleanup(func() {
		mgr.Shutdown()
	})

	cmd := fakeWorkerCmd(t, "invalid_response")

	// Worker sends garbage, should result in an error
	_, err := mgr.Execute(context.Background(), "invalid-key", cmd, []string{"test"}, nil)
	if err == nil {
		t.Error("Expected error for invalid response, got nil")
	}
}

func TestPersistentWorkerManager_CancelledRequest(t *testing.T) {
	mgr := NewPersistentWorkerManager(slog.Default())
	t.Cleanup(func() {
		mgr.Shutdown()
	})

	cmd := fakeWorkerCmd(t, "echo")

	// Send a request with cancel=true
	result, err := mgr.Execute(context.Background(), "cancel-key", cmd, []string{}, nil,
		WithCancel(true),
	)
	if err != nil {
		t.Fatalf("Execute() with cancel failed: %v", err)
	}
	if !result.WasCancelled {
		t.Error("Expected WasCancelled=true")
	}
}

func TestPersistentWorkerManager_Shutdown(t *testing.T) {
	mgr := NewPersistentWorkerManager(slog.Default())

	cmd := fakeWorkerCmd(t, "echo")
	_, err := mgr.Execute(context.Background(), "shutdown-key", cmd, []string{"work"}, nil)
	if err != nil {
		t.Fatalf("Execute() failed: %v", err)
	}

	mgr.mu.Lock()
	countBefore := len(mgr.workers)
	mgr.mu.Unlock()
	if countBefore != 1 {
		t.Errorf("Expected 1 worker before shutdown, got %d", countBefore)
	}

	mgr.Shutdown()

	mgr.mu.Lock()
	countAfter := len(mgr.workers)
	mgr.mu.Unlock()
	if countAfter != 0 {
		t.Errorf("Expected 0 workers after shutdown, got %d", countAfter)
	}
}

func TestPersistentWorkerManager_ContextCancel(t *testing.T) {
	mgr := NewPersistentWorkerManager(slog.Default())
	t.Cleanup(func() {
		mgr.Shutdown()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	cmd := fakeWorkerCmd(t, "slow") // 500ms delay

	_, err := mgr.Execute(ctx, "timeout-key", cmd, []string{"slow-work"}, nil)
	if err == nil {
		t.Error("Expected error from cancelled context, got nil")
	}
}

func TestPersistentWorkerManager_ConcurrentAccess(t *testing.T) {
	mgr := NewPersistentWorkerManager(slog.Default())
	t.Cleanup(func() {
		mgr.Shutdown()
	})

	const numGoroutines = 10
	const numRequests = 5

	var wg sync.WaitGroup
	errCh := make(chan error, numGoroutines*numRequests)

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := "concurrent-key" // All goroutines share the same key
			for j := 0; j < numRequests; j++ {
				cmd := fakeWorkerCmd(t, "echo")
				_, err := mgr.Execute(context.Background(), key, cmd, []string{"work"}, nil)
				if err != nil {
					errCh <- err
					return
				}
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("Concurrent access error: %v", err)
	}
}
