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

func TestWorkerPool_InputHardlinking(t *testing.T) {
	// Setup
	tempDir := t.TempDir()
	buildRoot := t.TempDir()

	localStore, err := storage.NewLocalStore(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to create local store: %v", err)
	}

	sched := scheduler.NewScheduler(100, "")
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

	// Create a command that checks if input.txt is NOT a symlink and IS a regular file
	command := &repb.Command{
		Arguments: []string{"sh", "-c", "[ ! -L input.txt ] && [ -f input.txt ] || exit 1"},
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
	op, err := sched.Enqueue(ctx, actionDigest.ToProto(), false, "")
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
		t.Errorf("Expected exit code 0 (meaning input.txt is NOT a symlink), got %d", finalState.Result.Result.ExitCode)
	}
}

func TestWorkerPool_InputHardlinking_Executable(t *testing.T) {
	// Setup
	tempDir := t.TempDir()
	buildRoot := t.TempDir()

	localStore, err := storage.NewLocalStore(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to create local store: %v", err)
	}

	sched := scheduler.NewScheduler(100, "")
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

	// Create an input file that is NOT executable in CAS
	inputContent := []byte("#!/bin/sh\necho executable content")
	inputDigest := digest.NewFromBlob(inputContent)
	if err := localStore.Put(ctx, inputDigest, bytes.NewReader(inputContent)); err != nil {
		t.Fatalf("Failed to upload input file: %v", err)
	}

	// Verify it's not executable initially
	path, _ := localStore.BlobPath(inputDigest)
	info, _ := os.Stat(path)
	if info.Mode()&0111 != 0 {
		// It might be executable depending on umask, but let's ensure it's not for the test
		os.Chmod(path, 0644)
	}

	// Create input root directory with the file, marked as EXECUTABLE
	inputDir := &repb.Directory{
		Files: []*repb.FileNode{
			{
				Name:         "script.sh",
				Digest:       inputDigest.ToProto(),
				IsExecutable: true,
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

	// Create a command that executes script.sh
	command := &repb.Command{
		Arguments: []string{"./script.sh"},
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
	op, err := sched.Enqueue(ctx, actionDigest.ToProto(), false, "")
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

	// Verify CAS file became executable
	info, _ = os.Stat(path)
	if info.Mode()&0111 == 0 {
		t.Error("CAS file should have been made executable")
	}
}
