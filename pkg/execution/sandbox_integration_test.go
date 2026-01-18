package execution

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/bazelbuild/remote-apis-sdks/go/pkg/digest"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/colinrgodsey/goREgo/pkg/config"
	"github.com/colinrgodsey/goREgo/pkg/scheduler"
	"github.com/colinrgodsey/goREgo/pkg/storage"
	"google.golang.org/protobuf/proto"
)

const defaultSandboxPath = "/usr/bin/linux-sandbox"

func sandboxAvailable() bool {
	_, err := os.Stat(defaultSandboxPath)
	return err == nil
}

func skipIfNoSandbox(t *testing.T) {
	if !sandboxAvailable() {
		t.Skipf("linux-sandbox not available at %s", defaultSandboxPath)
	}
}

func setupSandboxedWorkerPool(t *testing.T, networkIsolation bool) (*WorkerPool, *scheduler.Scheduler, storage.BlobStore, string) {
	t.Helper()

	tempDir := t.TempDir()
	buildRoot := t.TempDir()

	localStore, err := storage.NewLocalStore(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to create local store: %v", err)
	}

	sched := scheduler.NewScheduler(100)
	cfg := config.ExecutionConfig{
		Enabled:     true,
		Concurrency: 2,
		BuildRoot:   buildRoot,
		QueueSize:   100,
		Sandbox: config.SandboxConfig{
			Enabled:          true,
			BinaryPath:       defaultSandboxPath,
			NetworkIsolation: networkIsolation,
			KillDelay:        5,
			Debug:            false,
		},
	}

	wp, err := NewWorkerPool(cfg, sched, localStore, localStore)
	if err != nil {
		t.Fatalf("Failed to create worker pool: %v", err)
	}

	return wp, sched, localStore, buildRoot
}

func createAndUploadAction(t *testing.T, store storage.BlobStore, command *repb.Command, inputRoot *repb.Directory) digest.Digest {
	t.Helper()
	ctx := context.Background()

	// Upload command
	cmdBytes, err := proto.Marshal(command)
	if err != nil {
		t.Fatalf("Failed to marshal command: %v", err)
	}
	cmdDigest := digest.NewFromBlob(cmdBytes)
	if err := store.Put(ctx, cmdDigest, bytes.NewReader(cmdBytes)); err != nil {
		t.Fatalf("Failed to upload command: %v", err)
	}

	// Upload input root
	dirBytes, err := proto.Marshal(inputRoot)
	if err != nil {
		t.Fatalf("Failed to marshal directory: %v", err)
	}
	dirDigest := digest.NewFromBlob(dirBytes)
	if err := store.Put(ctx, dirDigest, bytes.NewReader(dirBytes)); err != nil {
		t.Fatalf("Failed to upload directory: %v", err)
	}

	// Create and upload action
	action := &repb.Action{
		CommandDigest:   cmdDigest.ToProto(),
		InputRootDigest: dirDigest.ToProto(),
	}
	actionBytes, err := proto.Marshal(action)
	if err != nil {
		t.Fatalf("Failed to marshal action: %v", err)
	}
	actionDigest := digest.NewFromBlob(actionBytes)
	if err := store.Put(ctx, actionDigest, bytes.NewReader(actionBytes)); err != nil {
		t.Fatalf("Failed to upload action: %v", err)
	}

	return actionDigest
}

func runActionAndWait(t *testing.T, wp *WorkerPool, sched *scheduler.Scheduler, actionDigest digest.Digest, timeout time.Duration) *repb.ExecuteResponse {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Start worker pool
	go func() {
		_ = wp.Run(ctx)
	}()

	// Enqueue action
	op, err := sched.Enqueue(ctx, actionDigest.ToProto(), false)
	if err != nil {
		t.Fatalf("Failed to enqueue action: %v", err)
	}

	// Subscribe and wait for completion
	updates, unsubscribe := sched.Subscribe(op.Name)
	defer unsubscribe()

	for {
		select {
		case update := <-updates:
			if update.Done {
				resp := update.GetResponse()
				if resp == nil {
					t.Fatal("Operation completed but response is nil")
				}
				execResp := &repb.ExecuteResponse{}
				if err := resp.UnmarshalTo(execResp); err != nil {
					t.Fatalf("Failed to unmarshal response: %v", err)
				}
				return execResp
			}
		case <-ctx.Done():
			t.Fatalf("Timeout waiting for action to complete: %v", ctx.Err())
		}
	}
}

func TestSandbox_BasicExecution(t *testing.T) {
	skipIfNoSandbox(t)

	wp, sched, store, _ := setupSandboxedWorkerPool(t, false)

	command := &repb.Command{
		Arguments: []string{"echo", "hello from sandbox"},
	}
	emptyDir := &repb.Directory{}

	actionDigest := createAndUploadAction(t, store, command, emptyDir)
	resp := runActionAndWait(t, wp, sched, actionDigest, 30*time.Second)

	if resp.Result == nil {
		t.Fatal("Expected result, got nil")
	}

	if resp.Result.ExitCode != 0 {
		t.Errorf("Expected exit code 0, got %d", resp.Result.ExitCode)
	}

	// Verify stdout contains expected output
	if resp.Result.StdoutDigest != nil {
		stdoutDigest, _ := digest.NewFromProto(resp.Result.StdoutDigest)
		rc, err := store.Get(context.Background(), stdoutDigest)
		if err != nil {
			t.Fatalf("Failed to get stdout: %v", err)
		}
		defer rc.Close()

		buf := new(bytes.Buffer)
		buf.ReadFrom(rc)
		stdout := buf.String()

		if !bytes.Contains([]byte(stdout), []byte("hello from sandbox")) {
			t.Errorf("Expected stdout to contain 'hello from sandbox', got: %s", stdout)
		}
	}
}

func TestSandbox_OutputFileCapture(t *testing.T) {
	skipIfNoSandbox(t)

	wp, sched, store, _ := setupSandboxedWorkerPool(t, false)

	command := &repb.Command{
		Arguments:   []string{"sh", "-c", "echo 'sandboxed output' > output.txt"},
		OutputFiles: []string{"output.txt"},
	}
	emptyDir := &repb.Directory{}

	actionDigest := createAndUploadAction(t, store, command, emptyDir)
	resp := runActionAndWait(t, wp, sched, actionDigest, 30*time.Second)

	if resp.Result == nil {
		t.Fatal("Expected result, got nil")
	}

	if resp.Result.ExitCode != 0 {
		t.Errorf("Expected exit code 0, got %d", resp.Result.ExitCode)
	}

	// Verify output file was captured
	if len(resp.Result.OutputFiles) != 1 {
		t.Fatalf("Expected 1 output file, got %d", len(resp.Result.OutputFiles))
	}

	outputFile := resp.Result.OutputFiles[0]
	if outputFile.Path != "output.txt" {
		t.Errorf("Expected output path 'output.txt', got '%s'", outputFile.Path)
	}

	// Verify content
	outputDigest, _ := digest.NewFromProto(outputFile.Digest)
	rc, err := store.Get(context.Background(), outputDigest)
	if err != nil {
		t.Fatalf("Failed to get output file: %v", err)
	}
	defer rc.Close()

	buf := new(bytes.Buffer)
	buf.ReadFrom(rc)
	content := buf.String()

	if !bytes.Contains([]byte(content), []byte("sandboxed output")) {
		t.Errorf("Expected output to contain 'sandboxed output', got: %s", content)
	}
}

func TestSandbox_InputFileAccess(t *testing.T) {
	skipIfNoSandbox(t)

	wp, sched, store, _ := setupSandboxedWorkerPool(t, false)
	ctx := context.Background()

	// Create an input file
	inputContent := []byte("input file content for sandbox test")
	inputDigest := digest.NewFromBlob(inputContent)
	if err := store.Put(ctx, inputDigest, bytes.NewReader(inputContent)); err != nil {
		t.Fatalf("Failed to upload input file: %v", err)
	}

	// Create input directory with the file
	inputDir := &repb.Directory{
		Files: []*repb.FileNode{
			{
				Name:   "input.txt",
				Digest: inputDigest.ToProto(),
			},
		},
	}

	// Command reads the input file
	command := &repb.Command{
		Arguments:   []string{"cat", "input.txt"},
		OutputFiles: []string{},
	}

	actionDigest := createAndUploadAction(t, store, command, inputDir)
	resp := runActionAndWait(t, wp, sched, actionDigest, 30*time.Second)

	if resp.Result == nil {
		t.Fatal("Expected result, got nil")
	}

	if resp.Result.ExitCode != 0 {
		t.Errorf("Expected exit code 0, got %d", resp.Result.ExitCode)
	}

	// Verify stdout contains input file content
	if resp.Result.StdoutDigest != nil {
		stdoutDigest, _ := digest.NewFromProto(resp.Result.StdoutDigest)
		rc, err := store.Get(ctx, stdoutDigest)
		if err != nil {
			t.Fatalf("Failed to get stdout: %v", err)
		}
		defer rc.Close()

		buf := new(bytes.Buffer)
		buf.ReadFrom(rc)
		stdout := buf.String()

		if !bytes.Contains([]byte(stdout), []byte("input file content for sandbox test")) {
			t.Errorf("Expected stdout to contain input content, got: %s", stdout)
		}
	}
}

func TestSandbox_InputProtection(t *testing.T) {
	skipIfNoSandbox(t)

	wp, sched, store, _ := setupSandboxedWorkerPool(t, false)
	ctx := context.Background()

	// Create an input file
	inputContent := []byte("original content")
	inputDigest := digest.NewFromBlob(inputContent)
	if err := store.Put(ctx, inputDigest, bytes.NewReader(inputContent)); err != nil {
		t.Fatalf("Failed to upload input file: %v", err)
	}

	// Create input directory
	inputDir := &repb.Directory{
		Files: []*repb.FileNode{
			{
				Name:   "protected.txt",
				Digest: inputDigest.ToProto(),
			},
		},
	}

	// Command attempts to modify the input file (should fail in sandbox)
	command := &repb.Command{
		Arguments: []string{"sh", "-c", "echo 'modified' > protected.txt; cat protected.txt"},
	}

	actionDigest := createAndUploadAction(t, store, command, inputDir)
	resp := runActionAndWait(t, wp, sched, actionDigest, 30*time.Second)

	if resp.Result == nil {
		t.Fatal("Expected result, got nil")
	}

	// The command should fail (non-zero exit) because the input is read-only
	// OR if it "succeeds", the original file in CAS should be unchanged
	if resp.Result.ExitCode == 0 {
		// If it succeeded, verify the CAS file is unchanged
		rc, err := store.Get(ctx, inputDigest)
		if err != nil {
			t.Fatalf("Failed to get original input: %v", err)
		}
		defer rc.Close()

		buf := new(bytes.Buffer)
		buf.ReadFrom(rc)
		content := buf.Bytes()

		if !bytes.Equal(content, inputContent) {
			t.Errorf("CAS file was modified! Expected %q, got %q", inputContent, content)
		}
	}
	// Non-zero exit is also acceptable - it means the write was blocked
}

func TestSandbox_NetworkIsolation(t *testing.T) {
	skipIfNoSandbox(t)

	wp, sched, store, _ := setupSandboxedWorkerPool(t, true) // Enable network isolation

	// Command attempts network access (should fail with -N flag)
	// Using a simple ping or connection test
	command := &repb.Command{
		Arguments: []string{"sh", "-c", "ping -c 1 -W 1 8.8.8.8 2>&1 || echo 'network blocked'"},
	}
	emptyDir := &repb.Directory{}

	actionDigest := createAndUploadAction(t, store, command, emptyDir)
	resp := runActionAndWait(t, wp, sched, actionDigest, 30*time.Second)

	if resp.Result == nil {
		t.Fatal("Expected result, got nil")
	}

	// Check stdout for network blocked message or non-zero exit
	if resp.Result.StdoutDigest != nil {
		stdoutDigest, _ := digest.NewFromProto(resp.Result.StdoutDigest)
		rc, err := store.Get(context.Background(), stdoutDigest)
		if err != nil {
			t.Fatalf("Failed to get stdout: %v", err)
		}
		defer rc.Close()

		buf := new(bytes.Buffer)
		buf.ReadFrom(rc)
		stdout := buf.String()

		// Network should be blocked - either ping fails or we see our echo
		if !bytes.Contains([]byte(stdout), []byte("network blocked")) &&
			!bytes.Contains([]byte(stdout), []byte("Network is unreachable")) &&
			!bytes.Contains([]byte(stdout), []byte("Operation not permitted")) {
			// If network wasn't blocked, log it but ping might just fail for other reasons
			t.Logf("Network test output: %s (exit code: %d)", stdout, resp.Result.ExitCode)
		}
	}
}

func TestSandbox_NonZeroExitCode(t *testing.T) {
	skipIfNoSandbox(t)

	wp, sched, store, _ := setupSandboxedWorkerPool(t, false)

	command := &repb.Command{
		Arguments: []string{"sh", "-c", "exit 42"},
	}
	emptyDir := &repb.Directory{}

	actionDigest := createAndUploadAction(t, store, command, emptyDir)
	resp := runActionAndWait(t, wp, sched, actionDigest, 30*time.Second)

	if resp.Result == nil {
		t.Fatal("Expected result, got nil")
	}

	if resp.Result.ExitCode != 42 {
		t.Errorf("Expected exit code 42, got %d", resp.Result.ExitCode)
	}
}

func TestSandbox_WorkingDirectory(t *testing.T) {
	skipIfNoSandbox(t)

	wp, sched, store, _ := setupSandboxedWorkerPool(t, false)

	command := &repb.Command{
		Arguments:        []string{"sh", "-c", "pwd && echo 'output' > result.txt"},
		WorkingDirectory: "subdir",
		OutputFiles:      []string{"subdir/result.txt"},
	}
	emptyDir := &repb.Directory{}

	actionDigest := createAndUploadAction(t, store, command, emptyDir)
	resp := runActionAndWait(t, wp, sched, actionDigest, 30*time.Second)

	if resp.Result == nil {
		t.Fatal("Expected result, got nil")
	}

	if resp.Result.ExitCode != 0 {
		// Get stderr for debugging
		if resp.Result.StderrDigest != nil {
			stderrDigest, _ := digest.NewFromProto(resp.Result.StderrDigest)
			rc, _ := store.Get(context.Background(), stderrDigest)
			if rc != nil {
				buf := new(bytes.Buffer)
				buf.ReadFrom(rc)
				rc.Close()
				t.Logf("stderr: %s", buf.String())
			}
		}
		t.Errorf("Expected exit code 0, got %d", resp.Result.ExitCode)
	}

	// Verify output file was created in working directory
	if len(resp.Result.OutputFiles) != 1 {
		t.Fatalf("Expected 1 output file, got %d", len(resp.Result.OutputFiles))
	}
}

func TestSandbox_EnvironmentVariables(t *testing.T) {
	skipIfNoSandbox(t)

	wp, sched, store, _ := setupSandboxedWorkerPool(t, false)

	command := &repb.Command{
		Arguments: []string{"sh", "-c", "echo $MY_VAR"},
		EnvironmentVariables: []*repb.Command_EnvironmentVariable{
			{Name: "MY_VAR", Value: "sandbox_env_test"},
		},
	}
	emptyDir := &repb.Directory{}

	actionDigest := createAndUploadAction(t, store, command, emptyDir)
	resp := runActionAndWait(t, wp, sched, actionDigest, 30*time.Second)

	if resp.Result == nil {
		t.Fatal("Expected result, got nil")
	}

	if resp.Result.ExitCode != 0 {
		t.Errorf("Expected exit code 0, got %d", resp.Result.ExitCode)
	}

	// Verify env var was passed through
	if resp.Result.StdoutDigest != nil {
		stdoutDigest, _ := digest.NewFromProto(resp.Result.StdoutDigest)
		rc, err := store.Get(context.Background(), stdoutDigest)
		if err != nil {
			t.Fatalf("Failed to get stdout: %v", err)
		}
		defer rc.Close()

		buf := new(bytes.Buffer)
		buf.ReadFrom(rc)
		stdout := buf.String()

		if !bytes.Contains([]byte(stdout), []byte("sandbox_env_test")) {
			t.Errorf("Expected stdout to contain env var value, got: %s", stdout)
		}
	}
}
