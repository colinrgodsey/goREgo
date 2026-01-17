package execution

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/bazelbuild/remote-apis-sdks/go/pkg/digest"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/colinrgodsey/goREgo/pkg/config"
	"github.com/colinrgodsey/goREgo/pkg/scheduler"
	"github.com/colinrgodsey/goREgo/pkg/storage"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	defaultTimeout = 10 * time.Minute
)

// WorkerPool manages a pool of workers that execute actions.
type WorkerPool struct {
	scheduler   *scheduler.Scheduler
	blobStore   storage.BlobStore
	actionCache storage.ActionCache
	buildRoot   string
	concurrency int
	tracer      trace.Tracer
	logger      *slog.Logger
}

// NewWorkerPool creates a new WorkerPool.
func NewWorkerPool(cfg config.ExecutionConfig, sched *scheduler.Scheduler, blobStore storage.BlobStore, actionCache storage.ActionCache) *WorkerPool {
	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = runtime.NumCPU()
	}

	return &WorkerPool{
		scheduler:   sched,
		blobStore:   blobStore,
		actionCache: actionCache,
		buildRoot:   cfg.BuildRoot,
		concurrency: concurrency,
		tracer:      otel.Tracer("gorego/pkg/execution"),
		logger:      slog.Default().With("component", "worker"),
	}
}

// Run starts the worker pool. It blocks until the context is cancelled.
func (w *WorkerPool) Run(ctx context.Context) error {
	if err := os.MkdirAll(w.buildRoot, 0755); err != nil {
		return fmt.Errorf("failed to create build root: %w", err)
	}

	g, ctx := errgroup.WithContext(ctx)

	for i := 0; i < w.concurrency; i++ {
		workerID := i
		g.Go(func() error {
			return w.runWorker(ctx, workerID)
		})
	}

	w.logger.Info("worker pool started", "concurrency", w.concurrency)
	return g.Wait()
}

func (w *WorkerPool) runWorker(ctx context.Context, workerID int) error {
	logger := w.logger.With("worker_id", workerID)

	for {
		task, err := w.scheduler.Dequeue(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil // Graceful shutdown
			}
			logger.Error("failed to dequeue task", "error", err)
			continue
		}

		logger.Info("processing task", "operation_id", task.OperationID)
		w.executeTask(ctx, task)
	}
}

func (w *WorkerPool) executeTask(ctx context.Context, task *scheduler.Task) {
	ctx, span := w.tracer.Start(ctx, "worker.executeTask",
		trace.WithAttributes(attribute.String("operation_id", task.OperationID)))
	defer span.End()

	name := fmt.Sprintf("operations/%s", task.OperationID)

	// Update state to EXECUTING
	w.scheduler.UpdateState(name, scheduler.StateExecuting, &repb.ExecuteOperationMetadata{
		Stage:        repb.ExecutionStage_EXECUTING,
		ActionDigest: task.ActionDigest,
	})

	result, err := w.execute(ctx, task)
	if err != nil {
		span.RecordError(err)
		w.scheduler.Fail(name, err)
		return
	}

	w.scheduler.Complete(name, &repb.ExecuteResponse{
		Result:       result,
		CachedResult: false,
	})
}

func (w *WorkerPool) execute(ctx context.Context, task *scheduler.Task) (*repb.ActionResult, error) {
	ctx, span := w.tracer.Start(ctx, "worker.execute")
	defer span.End()

	// 1. Fetch Action proto from CAS
	actionDigest, err := digest.NewFromProto(task.ActionDigest)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid action digest: %v", err)
	}

	action, err := w.fetchAction(ctx, actionDigest)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch action: %w", err)
	}

	// 2. Fetch Command proto from CAS
	cmdDigest, err := digest.NewFromProto(action.CommandDigest)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid command digest: %v", err)
	}

	command, err := w.fetchCommand(ctx, cmdDigest)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch command: %w", err)
	}

	// 3. Fetch input root Directory proto
	inputRootDigest, err := digest.NewFromProto(action.InputRootDigest)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid input root digest: %v", err)
	}

	// 4. Create working directory
	workDir := filepath.Join(w.buildRoot, task.OperationID)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create work dir: %w", err)
	}
	defer os.RemoveAll(workDir) // Cleanup

	// 5. Stage inputs
	if err := w.stageInputs(ctx, workDir, inputRootDigest); err != nil {
		return nil, fmt.Errorf("failed to stage inputs: %w", err)
	}

	w.logger.Debug("Creating output directories",
		"output_files", command.OutputFiles,
		"output_directories", command.OutputDirectories,
		"output_paths", command.OutputPaths)

	// 5.5 Create output directories
	for _, outputFile := range command.OutputFiles {
		// Output files are relative to the working directory (which is inside workDir)
		path := filepath.Join(workDir, command.WorkingDirectory, outputFile)
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create output parent dir %s: %w", dir, err)
		}
	}

	for _, outputDir := range command.OutputDirectories {
		path := filepath.Join(workDir, command.WorkingDirectory, outputDir)
		if err := os.MkdirAll(path, 0755); err != nil {
			return nil, fmt.Errorf("failed to create output dir %s: %w", path, err)
		}
	}

	for _, outputPath := range command.OutputPaths {
		path := filepath.Join(workDir, command.WorkingDirectory, outputPath)
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create output parent dir %s: %w", dir, err)
		}
	}

	// 6. Execute command
	execResult, err := w.runCommand(ctx, workDir, command, action.Timeout)
	if err != nil {
		return nil, fmt.Errorf("failed to execute command: %w", err)
	}

	// 7. Upload outputs
	result, err := w.uploadOutputs(ctx, workDir, command, execResult)
	if err != nil {
		return nil, fmt.Errorf("failed to upload outputs: %w", err)
	}

	// 8. Update action cache if successful
	if result.ExitCode == 0 && !task.SkipCache {
		_ = w.actionCache.UpdateActionResult(ctx, actionDigest, result)
	}

	return result, nil
}

func (w *WorkerPool) fetchAction(ctx context.Context, d digest.Digest) (*repb.Action, error) {
	rc, err := w.blobStore.Get(ctx, d)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}

	var action repb.Action
	if err := proto.Unmarshal(data, &action); err != nil {
		return nil, err
	}
	return &action, nil
}

func (w *WorkerPool) fetchCommand(ctx context.Context, d digest.Digest) (*repb.Command, error) {
	rc, err := w.blobStore.Get(ctx, d)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}

	var cmd repb.Command
	if err := proto.Unmarshal(data, &cmd); err != nil {
		return nil, err
	}
	return &cmd, nil
}

func (w *WorkerPool) fetchDirectory(ctx context.Context, d digest.Digest) (*repb.Directory, error) {
	rc, err := w.blobStore.Get(ctx, d)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}

	var dir repb.Directory
	if err := proto.Unmarshal(data, &dir); err != nil {
		return nil, err
	}
	return &dir, nil
}

func (w *WorkerPool) stageInputs(ctx context.Context, workDir string, inputRootDigest digest.Digest) error {
	ctx, span := w.tracer.Start(ctx, "worker.stageInputs")
	defer span.End()

	return w.stageDirectory(ctx, workDir, inputRootDigest)
}

func (w *WorkerPool) stageDirectory(ctx context.Context, targetDir string, dirDigest digest.Digest) error {
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}

	dir, err := w.fetchDirectory(ctx, dirDigest)
	if err != nil {
		return err
	}

	// Stage files
	for _, file := range dir.Files {
		filePath := filepath.Join(targetDir, file.Name)
		fileDigest, err := digest.NewFromProto(file.Digest)
		if err != nil {
			return err
		}

		if err := w.stageFile(ctx, filePath, fileDigest, file.IsExecutable); err != nil {
			return fmt.Errorf("failed to stage file %s: %w", file.Name, err)
		}
	}

	// Stage symlinks
	for _, symlink := range dir.Symlinks {
		linkPath := filepath.Join(targetDir, symlink.Name)
		if err := os.Symlink(symlink.Target, linkPath); err != nil {
			return fmt.Errorf("failed to create symlink %s: %w", symlink.Name, err)
		}
	}

	// Recurse into subdirectories
	for _, subdir := range dir.Directories {
		subdirPath := filepath.Join(targetDir, subdir.Name)
		subdirDigest, err := digest.NewFromProto(subdir.Digest)
		if err != nil {
			return err
		}
		if err := w.stageDirectory(ctx, subdirPath, subdirDigest); err != nil {
			return err
		}
	}

	return nil
}

func (w *WorkerPool) stageFile(ctx context.Context, targetPath string, d digest.Digest, executable bool) error {
	rc, err := w.blobStore.Get(ctx, d)
	if err != nil {
		return err
	}
	defer rc.Close()

	perm := os.FileMode(0644)
	if executable {
		perm = 0755
	}

	f, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, rc)
	return err
}

type execResult struct {
	exitCode int
	stdout   []byte
	stderr   []byte
	duration time.Duration
}

func (w *WorkerPool) runCommand(ctx context.Context, workDir string, command *repb.Command, timeout *durationpb.Duration) (*execResult, error) {
	ctx, span := w.tracer.Start(ctx, "worker.runCommand")
	defer span.End()

	// Determine timeout
	cmdTimeout := defaultTimeout
	if timeout != nil {
		cmdTimeout = timeout.AsDuration()
	}

	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()

	if len(command.Arguments) == 0 {
		return nil, status.Error(codes.InvalidArgument, "command has no arguments")
	}

	cmd := exec.CommandContext(ctx, command.Arguments[0], command.Arguments[1:]...)
	cmd.Dir = filepath.Join(workDir, command.WorkingDirectory)

	// Set environment
	env := os.Environ() // Start with current env
	for _, ev := range command.EnvironmentVariables {
		env = append(env, fmt.Sprintf("%s=%s", ev.Name, ev.Value))
	}
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	w.logger.Debug("Executing command", "dir", cmd.Dir, "args", cmd.Args)

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)

	result := &execResult{
		exitCode: 0,
		stdout:   stdout.Bytes(),
		stderr:   stderr.Bytes(),
		duration: duration,
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.exitCode = exitErr.ExitCode()
		} else if ctx.Err() == context.DeadlineExceeded {
			return nil, status.Error(codes.DeadlineExceeded, "command timed out")
		} else {
			return nil, fmt.Errorf("command failed: %w", err)
		}
	}

	span.SetAttributes(
		attribute.Int("exit_code", result.exitCode),
		attribute.Int64("duration_ms", duration.Milliseconds()),
	)

	return result, nil
}

func (w *WorkerPool) uploadOutputs(ctx context.Context, workDir string, command *repb.Command, execResult *execResult) (*repb.ActionResult, error) {
	ctx, span := w.tracer.Start(ctx, "worker.uploadOutputs")
	defer span.End()

	result := &repb.ActionResult{
		ExitCode: int32(execResult.exitCode),
		ExecutionMetadata: &repb.ExecutedActionMetadata{
			ExecutionCompletedTimestamp: nil, // TODO: timestamps
		},
	}

	// Upload stdout
	if len(execResult.stdout) > 0 {
		d, err := w.uploadBlob(ctx, execResult.stdout)
		if err != nil {
			return nil, fmt.Errorf("failed to upload stdout: %w", err)
		}
		result.StdoutDigest = d.ToProto()
	}

	// Upload stderr
	if len(execResult.stderr) > 0 {
		d, err := w.uploadBlob(ctx, execResult.stderr)
		if err != nil {
			return nil, fmt.Errorf("failed to upload stderr: %w", err)
		}
		result.StderrDigest = d.ToProto()
	}

	// Upload output files
	for _, outputPath := range command.OutputFiles {
		fullPath := filepath.Join(workDir, command.WorkingDirectory, outputPath)
		info, err := os.Stat(fullPath)
		if os.IsNotExist(err) {
			continue // Output not produced
		}
		if err != nil {
			return nil, fmt.Errorf("failed to stat output %s: %w", outputPath, err)
		}

		data, err := os.ReadFile(fullPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read output %s: %w", outputPath, err)
		}

		d, err := w.uploadBlob(ctx, data)
		if err != nil {
			return nil, fmt.Errorf("failed to upload output %s: %w", outputPath, err)
		}

		result.OutputFiles = append(result.OutputFiles, &repb.OutputFile{
			Path:         outputPath,
			Digest:       d.ToProto(),
			IsExecutable: info.Mode()&0111 != 0,
		})
	}

	// Upload output directories
	for _, outputPath := range command.OutputDirectories {
		fullPath := filepath.Join(workDir, command.WorkingDirectory, outputPath)
		if _, err := os.Stat(fullPath); os.IsNotExist(err) {
			continue // Output not produced
		}

		tree, rootDigest, err := w.buildTree(ctx, fullPath)
		if err != nil {
			return nil, fmt.Errorf("failed to build tree for %s: %w", outputPath, err)
		}

		// Upload the Tree proto
		treeData, err := proto.Marshal(tree)
		if err != nil {
			return nil, err
		}
		treeDigest, err := w.uploadBlob(ctx, treeData)
		if err != nil {
			return nil, fmt.Errorf("failed to upload tree for %s: %w", outputPath, err)
		}

		result.OutputDirectories = append(result.OutputDirectories, &repb.OutputDirectory{
			Path:                outputPath,
			TreeDigest:          treeDigest.ToProto(),
			RootDirectoryDigest: rootDigest.ToProto(),
		})
	}

	// Upload output paths (files or directories)
	for _, outputPath := range command.OutputPaths {
		fullPath := filepath.Join(workDir, command.WorkingDirectory, outputPath)
		info, err := os.Stat(fullPath)
		if os.IsNotExist(err) {
			continue // Output not produced
		}
		if err != nil {
			return nil, fmt.Errorf("failed to stat output %s: %w", outputPath, err)
		}

		if info.IsDir() {
			// Handle as directory
			tree, rootDigest, err := w.buildTree(ctx, fullPath)
			if err != nil {
				return nil, fmt.Errorf("failed to build tree for %s: %w", outputPath, err)
			}

			// Upload the Tree proto
			treeData, err := proto.Marshal(tree)
			if err != nil {
				return nil, err
			}
			treeDigest, err := w.uploadBlob(ctx, treeData)
			if err != nil {
				return nil, fmt.Errorf("failed to upload tree for %s: %w", outputPath, err)
			}

			result.OutputDirectories = append(result.OutputDirectories, &repb.OutputDirectory{
				Path:                outputPath,
				TreeDigest:          treeDigest.ToProto(),
				RootDirectoryDigest: rootDigest.ToProto(),
			})
		} else {
			// Handle as file
			data, err := os.ReadFile(fullPath)
			if err != nil {
				return nil, fmt.Errorf("failed to read output %s: %w", outputPath, err)
			}

			d, err := w.uploadBlob(ctx, data)
			if err != nil {
				return nil, fmt.Errorf("failed to upload output %s: %w", outputPath, err)
			}

			result.OutputFiles = append(result.OutputFiles, &repb.OutputFile{
				Path:         outputPath,
				Digest:       d.ToProto(),
				IsExecutable: info.Mode()&0111 != 0,
			})
		}
	}

	return result, nil
}

func (w *WorkerPool) uploadBlob(ctx context.Context, data []byte) (digest.Digest, error) {
	d := digest.NewFromBlob(data)
	if err := w.blobStore.Put(ctx, d, bytes.NewReader(data)); err != nil {
		return digest.Digest{}, err
	}
	return d, nil
}

func (w *WorkerPool) buildTree(ctx context.Context, dirPath string) (*repb.Tree, digest.Digest, error) {
	tree := &repb.Tree{}

	rootDir, err := w.buildDirectoryProto(ctx, dirPath, tree)
	if err != nil {
		return nil, digest.Digest{}, err
	}

	tree.Root = rootDir

	// Compute root digest
	rootData, err := proto.Marshal(rootDir)
	if err != nil {
		return nil, digest.Digest{}, err
	}
	rootDigest := digest.NewFromBlob(rootData)

	// Upload root directory
	if err := w.blobStore.Put(ctx, rootDigest, bytes.NewReader(rootData)); err != nil {
		return nil, digest.Digest{}, err
	}

	return tree, rootDigest, nil
}

func (w *WorkerPool) buildDirectoryProto(ctx context.Context, dirPath string, tree *repb.Tree) (*repb.Directory, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, err
	}

	dir := &repb.Directory{}

	for _, entry := range entries {
		entryPath := filepath.Join(dirPath, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}

		if entry.IsDir() {
			subdir, err := w.buildDirectoryProto(ctx, entryPath, tree)
			if err != nil {
				return nil, err
			}

			// Upload subdirectory
			subdirData, err := proto.Marshal(subdir)
			if err != nil {
				return nil, err
			}
			subdirDigest, err := w.uploadBlob(ctx, subdirData)
			if err != nil {
				return nil, err
			}

			dir.Directories = append(dir.Directories, &repb.DirectoryNode{
				Name:   entry.Name(),
				Digest: subdirDigest.ToProto(),
			})
			tree.Children = append(tree.Children, subdir)
		} else if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(entryPath)
			if err != nil {
				return nil, err
			}
			dir.Symlinks = append(dir.Symlinks, &repb.SymlinkNode{
				Name:   entry.Name(),
				Target: target,
			})
		} else {
			data, err := os.ReadFile(entryPath)
			if err != nil {
				return nil, err
			}
			d, err := w.uploadBlob(ctx, data)
			if err != nil {
				return nil, err
			}
			dir.Files = append(dir.Files, &repb.FileNode{
				Name:         entry.Name(),
				Digest:       d.ToProto(),
				IsExecutable: info.Mode()&0111 != 0,
			})
		}
	}

	return dir, nil
}
