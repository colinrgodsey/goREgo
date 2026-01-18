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

func TestWorkerPool_Execute(t *testing.T) {
	// Setup
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
	}

	wp, err := NewWorkerPool(cfg, sched, localStore, localStore)
	if err != nil {
		t.Fatalf("Failed to create worker pool: %v", err)
	}

	// Create a simple "echo hello" command
	command := &repb.Command{
		Arguments: []string{"echo", "hello"},
	}
	cmdBytes, err := proto.Marshal(command)
	if err != nil {
		t.Fatalf("Failed to marshal command: %v", err)
	}
	cmdDigest := digest.NewFromBlob(cmdBytes)

	// Upload command to CAS
	if err := localStore.Put(context.Background(), cmdDigest, bytes.NewReader(cmdBytes)); err != nil {
		t.Fatalf("Failed to upload command: %v", err)
	}

	// Create empty input root directory
	emptyDir := &repb.Directory{}
	dirBytes, err := proto.Marshal(emptyDir)
	if err != nil {
		t.Fatalf("Failed to marshal directory: %v", err)
	}
	dirDigest := digest.NewFromBlob(dirBytes)

	if err := localStore.Put(context.Background(), dirDigest, bytes.NewReader(dirBytes)); err != nil {
		t.Fatalf("Failed to upload directory: %v", err)
	}

	// Create action
	action := &repb.Action{
		CommandDigest:   cmdDigest.ToProto(),
		InputRootDigest: dirDigest.ToProto(),
	}
	actionBytes, err := proto.Marshal(action)
	if err != nil {
		t.Fatalf("Failed to marshal action: %v", err)
	}
	actionDigest := digest.NewFromBlob(actionBytes)

	if err := localStore.Put(context.Background(), actionDigest, bytes.NewReader(actionBytes)); err != nil {
		t.Fatalf("Failed to upload action: %v", err)
	}

	// Start worker pool in background
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		_ = wp.Run(ctx)
	}()

	// Enqueue the action
	op, err := sched.Enqueue(ctx, actionDigest.ToProto(), false)
	if err != nil {
		t.Fatalf("Failed to enqueue action: %v", err)
	}

	// Subscribe and wait for completion
	updates, unsubscribe := sched.Subscribe(op.Name)
	defer unsubscribe()

	var finalState *scheduler.OperationStatus
	timeout := time.After(5 * time.Second)

	for {
		select {
		case update := <-updates:
			if update.State == scheduler.StateCompleted {
				finalState = update
				goto done
			}
		case <-timeout:
			t.Fatal("Timed out waiting for execution to complete")
		}
	}

done:
	if finalState == nil {
		t.Fatal("No final state received")
	}

	if finalState.Error != nil {
		t.Fatalf("Execution failed with error: %v", finalState.Error)
	}

	if finalState.Result == nil {
		t.Fatal("No result in final state")
	}

	if finalState.Result.Result.ExitCode != 0 {
		t.Errorf("Expected exit code 0, got %d", finalState.Result.Result.ExitCode)
	}

	// Verify stdout was uploaded
	if finalState.Result.Result.StdoutDigest == nil {
		t.Error("Expected stdout digest to be set")
	} else {
		stdoutDigest, err := digest.NewFromProto(finalState.Result.Result.StdoutDigest)
		if err != nil {
			t.Fatalf("Invalid stdout digest: %v", err)
		}

		rc, err := localStore.Get(ctx, stdoutDigest)
		if err != nil {
			t.Fatalf("Failed to get stdout: %v", err)
		}
		defer rc.Close()

		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(rc)
		stdout := buf.String()

		if stdout != "hello\n" {
			t.Errorf("Expected stdout 'hello\\n', got %q", stdout)
		}
	}
}

func TestWorkerPool_ExecuteWithOutputFile(t *testing.T) {
	// Setup
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
	}

	wp, err := NewWorkerPool(cfg, sched, localStore, localStore)
	if err != nil {
		t.Fatalf("Failed to create worker pool: %v", err)
	}

	// Create a command that writes to a file
	command := &repb.Command{
		Arguments:   []string{"sh", "-c", "echo 'output content' > output.txt"},
		OutputFiles: []string{"output.txt"},
	}
	cmdBytes, err := proto.Marshal(command)
	if err != nil {
		t.Fatalf("Failed to marshal command: %v", err)
	}
	cmdDigest := digest.NewFromBlob(cmdBytes)

	if err := localStore.Put(context.Background(), cmdDigest, bytes.NewReader(cmdBytes)); err != nil {
		t.Fatalf("Failed to upload command: %v", err)
	}

	// Create empty input root directory
	emptyDir := &repb.Directory{}
	dirBytes, err := proto.Marshal(emptyDir)
	if err != nil {
		t.Fatalf("Failed to marshal directory: %v", err)
	}
	dirDigest := digest.NewFromBlob(dirBytes)

	if err := localStore.Put(context.Background(), dirDigest, bytes.NewReader(dirBytes)); err != nil {
		t.Fatalf("Failed to upload directory: %v", err)
	}

	// Create action
	action := &repb.Action{
		CommandDigest:   cmdDigest.ToProto(),
		InputRootDigest: dirDigest.ToProto(),
	}
	actionBytes, err := proto.Marshal(action)
	if err != nil {
		t.Fatalf("Failed to marshal action: %v", err)
	}
	actionDigest := digest.NewFromBlob(actionBytes)

	if err := localStore.Put(context.Background(), actionDigest, bytes.NewReader(actionBytes)); err != nil {
		t.Fatalf("Failed to upload action: %v", err)
	}

	// Start worker pool in background
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		_ = wp.Run(ctx)
	}()

	// Enqueue the action
	op, err := sched.Enqueue(ctx, actionDigest.ToProto(), false)
	if err != nil {
		t.Fatalf("Failed to enqueue action: %v", err)
	}

	// Subscribe and wait for completion
	updates, unsubscribe := sched.Subscribe(op.Name)
	defer unsubscribe()

	var finalState *scheduler.OperationStatus
	timeout := time.After(5 * time.Second)

	for {
		select {
		case update := <-updates:
			if update.State == scheduler.StateCompleted {
				finalState = update
				goto done
			}
		case <-timeout:
			t.Fatal("Timed out waiting for execution to complete")
		}
	}

done:
	if finalState == nil {
		t.Fatal("No final state received")
	}

	if finalState.Error != nil {
		t.Fatalf("Execution failed with error: %v", finalState.Error)
	}

	if finalState.Result == nil || finalState.Result.Result == nil {
		t.Fatal("No result in final state")
	}

	if finalState.Result.Result.ExitCode != 0 {
		t.Errorf("Expected exit code 0, got %d", finalState.Result.Result.ExitCode)
	}

	// Verify output file was captured
	if len(finalState.Result.Result.OutputFiles) != 1 {
		t.Errorf("Expected 1 output file, got %d", len(finalState.Result.Result.OutputFiles))
	} else {
		outputFile := finalState.Result.Result.OutputFiles[0]
		if outputFile.Path != "output.txt" {
			t.Errorf("Expected path 'output.txt', got %q", outputFile.Path)
		}

		outputDigest, err := digest.NewFromProto(outputFile.Digest)
		if err != nil {
			t.Fatalf("Invalid output digest: %v", err)
		}

		rc, err := localStore.Get(ctx, outputDigest)
		if err != nil {
			t.Fatalf("Failed to get output file: %v", err)
		}
		defer rc.Close()

		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(rc)
		content := buf.String()

		if content != "output content\n" {
			t.Errorf("Expected output content 'output content\\n', got %q", content)
		}
	}
}

func TestWorkerPool_NonZeroExitCode(t *testing.T) {
	// Setup
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
	}

	wp, err := NewWorkerPool(cfg, sched, localStore, localStore)
	if err != nil {
		t.Fatalf("Failed to create worker pool: %v", err)
	}

	// Create a command that exits with non-zero code
	command := &repb.Command{
		Arguments: []string{"sh", "-c", "exit 42"},
	}
	cmdBytes, err := proto.Marshal(command)
	if err != nil {
		t.Fatalf("Failed to marshal command: %v", err)
	}
	cmdDigest := digest.NewFromBlob(cmdBytes)

	if err := localStore.Put(context.Background(), cmdDigest, bytes.NewReader(cmdBytes)); err != nil {
		t.Fatalf("Failed to upload command: %v", err)
	}

	// Create empty input root directory
	emptyDir := &repb.Directory{}
	dirBytes, err := proto.Marshal(emptyDir)
	if err != nil {
		t.Fatalf("Failed to marshal directory: %v", err)
	}
	dirDigest := digest.NewFromBlob(dirBytes)

	if err := localStore.Put(context.Background(), dirDigest, bytes.NewReader(dirBytes)); err != nil {
		t.Fatalf("Failed to upload directory: %v", err)
	}

	// Create action
	action := &repb.Action{
		CommandDigest:   cmdDigest.ToProto(),
		InputRootDigest: dirDigest.ToProto(),
	}
	actionBytes, err := proto.Marshal(action)
	if err != nil {
		t.Fatalf("Failed to marshal action: %v", err)
	}
	actionDigest := digest.NewFromBlob(actionBytes)

	if err := localStore.Put(context.Background(), actionDigest, bytes.NewReader(actionBytes)); err != nil {
		t.Fatalf("Failed to upload action: %v", err)
	}

	// Start worker pool in background
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		_ = wp.Run(ctx)
	}()

	// Enqueue the action
	op, err := sched.Enqueue(ctx, actionDigest.ToProto(), false)
	if err != nil {
		t.Fatalf("Failed to enqueue action: %v", err)
	}

	// Subscribe and wait for completion
	updates, unsubscribe := sched.Subscribe(op.Name)
	defer unsubscribe()

	var finalState *scheduler.OperationStatus
	timeout := time.After(5 * time.Second)

	for {
		select {
		case update := <-updates:
			if update.State == scheduler.StateCompleted {
				finalState = update
				goto done
			}
		case <-timeout:
			t.Fatal("Timed out waiting for execution to complete")
		}
	}

done:
	if finalState == nil {
		t.Fatal("No final state received")
	}

	if finalState.Error != nil {
		t.Fatalf("Execution should complete without error, got: %v", finalState.Error)
	}

	if finalState.Result == nil || finalState.Result.Result == nil {
		t.Fatal("No result in final state")
	}

	// Should have captured exit code 42
	if finalState.Result.Result.ExitCode != 42 {
		t.Errorf("Expected exit code 42, got %d", finalState.Result.Result.ExitCode)
	}
}

func TestWorkerPool_InputMaterialization(t *testing.T) {
	// Setup
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
	}

	wp, err := NewWorkerPool(cfg, sched, localStore, localStore)
	if err != nil {
		t.Fatalf("Failed to create worker pool: %v", err)
	}
	ctx := context.Background()

	// Create an input file
	inputContent := []byte("input file content")
	inputDigest := digest.NewFromBlob(inputContent)
	if err := localStore.Put(ctx, inputDigest, bytes.NewReader(inputContent)); err != nil {
		t.Fatalf("Failed to upload input file: %v", err)
	}

	// Create input root directory with the file
	inputDir := &repb.Directory{
		Files: []*repb.FileNode{
			{
				Name:         "input.txt",
				Digest:       inputDigest.ToProto(),
				IsExecutable: false,
			},
		},
	}
	dirBytes, err := proto.Marshal(inputDir)
	if err != nil {
		t.Fatalf("Failed to marshal directory: %v", err)
	}
	dirDigest := digest.NewFromBlob(dirBytes)

	if err := localStore.Put(ctx, dirDigest, bytes.NewReader(dirBytes)); err != nil {
		t.Fatalf("Failed to upload directory: %v", err)
	}

	// Create a command that reads input and writes to output
	command := &repb.Command{
		Arguments:   []string{"sh", "-c", "cat input.txt > output.txt"},
		OutputFiles: []string{"output.txt"},
	}
	cmdBytes, err := proto.Marshal(command)
	if err != nil {
		t.Fatalf("Failed to marshal command: %v", err)
	}
	cmdDigest := digest.NewFromBlob(cmdBytes)

	if err := localStore.Put(ctx, cmdDigest, bytes.NewReader(cmdBytes)); err != nil {
		t.Fatalf("Failed to upload command: %v", err)
	}

	// Create action
	action := &repb.Action{
		CommandDigest:   cmdDigest.ToProto(),
		InputRootDigest: dirDigest.ToProto(),
	}
	actionBytes, err := proto.Marshal(action)
	if err != nil {
		t.Fatalf("Failed to marshal action: %v", err)
	}
	actionDigest := digest.NewFromBlob(actionBytes)

	if err := localStore.Put(ctx, actionDigest, bytes.NewReader(actionBytes)); err != nil {
		t.Fatalf("Failed to upload action: %v", err)
	}

	// Start worker pool in background
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		_ = wp.Run(ctx)
	}()

	// Enqueue the action
	op, err := sched.Enqueue(ctx, actionDigest.ToProto(), false)
	if err != nil {
		t.Fatalf("Failed to enqueue action: %v", err)
	}

	// Subscribe and wait for completion
	updates, unsubscribe := sched.Subscribe(op.Name)
	defer unsubscribe()

	var finalState *scheduler.OperationStatus
	timeout := time.After(5 * time.Second)

	for {
		select {
		case update := <-updates:
			if update.State == scheduler.StateCompleted {
				finalState = update
				goto done
			}
		case <-timeout:
			t.Fatal("Timed out waiting for execution to complete")
		}
	}

done:
	if finalState == nil {
		t.Fatal("No final state received")
	}

	if finalState.Error != nil {
		t.Fatalf("Execution failed with error: %v", finalState.Error)
	}

	if finalState.Result == nil || finalState.Result.Result == nil {
		t.Fatal("No result in final state")
	}

	if finalState.Result.Result.ExitCode != 0 {
		t.Errorf("Expected exit code 0, got %d", finalState.Result.Result.ExitCode)
	}

	// Verify output file was captured with correct content
	if len(finalState.Result.Result.OutputFiles) != 1 {
		t.Errorf("Expected 1 output file, got %d", len(finalState.Result.Result.OutputFiles))
	} else {
		outputDigest, err := digest.NewFromProto(finalState.Result.Result.OutputFiles[0].Digest)
		if err != nil {
			t.Fatalf("Invalid output digest: %v", err)
		}

		rc, err := localStore.Get(ctx, outputDigest)
		if err != nil {
			t.Fatalf("Failed to get output file: %v", err)
		}
		defer rc.Close()

		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(rc)
		content := buf.String()

		// The output should match the input
		if content != string(inputContent) {
			t.Errorf("Expected output content %q, got %q", string(inputContent), content)
		}
	}

	// Verify build directory was cleaned up
	entries, err := os.ReadDir(buildRoot)
	if err != nil {
		t.Fatalf("Failed to read build root: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("Build directory should be cleaned up, but found %d entries", len(entries))
	}
}
