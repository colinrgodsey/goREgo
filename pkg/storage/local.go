package storage

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/protobuf/proto"
)

// OnPutCallback is called after a file is successfully added to the cache.
// It is used to trigger cache management tasks like janitor cleanup.
type OnPutCallback func()

type LocalStore struct {
	rootDir          string
	forceUpdateATime bool
	logger           *slog.Logger
	onPutCallback    OnPutCallback
}

func NewLocalStore(rootDir string, forceUpdateATime bool) (*LocalStore, error) {
	if err := os.MkdirAll(filepath.Join(rootDir, "cas"), 0755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(rootDir, "ac"), 0755); err != nil {
		return nil, err
	}
	return &LocalStore{
		rootDir:          rootDir,
		forceUpdateATime: forceUpdateATime,
		logger:           slog.Default().With("component", "localstore"),
	}, nil
}

// SetOnPutCallback sets a callback that is invoked after a file is
// successfully added to the cache via Put or PutFile.
func (s *LocalStore) SetOnPutCallback(cb OnPutCallback) {
	s.onPutCallback = cb
}

func (s *LocalStore) BlobPath(digest Digest) (string, error) {
	// data/cas/{ab}/{cd}/{digest}
	return filepath.Join(
		s.rootDir,
		"cas",
		digest.Hash[0:2],
		digest.Hash[2:4],
		digest.Hash,
	), nil
}

func (s *LocalStore) Has(ctx context.Context, digest Digest) (bool, error) {
	path, _ := s.BlobPath(digest)
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	if s.forceUpdateATime {
		now := time.Now()
		_ = os.Chtimes(path, now, now)
	}

	return true, nil
}

func (s *LocalStore) Get(ctx context.Context, digest Digest) (io.ReadCloser, error) {
	path, _ := s.BlobPath(digest)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	if s.forceUpdateATime {
		now := time.Now()
		_ = os.Chtimes(path, now, now)
	}

	return f, nil
}

func (s *LocalStore) getActionPath(digest Digest) string {
	return filepath.Join(
		s.rootDir,
		"ac",
		digest.Hash[0:2],
		digest.Hash[2:4],
		digest.Hash,
	)
}

func (s *LocalStore) GetActionResult(ctx context.Context, digest Digest) (*repb.ActionResult, error) {
	data, err := os.ReadFile(s.getActionPath(digest))
	if err != nil {
		return nil, err
	}

	var result repb.ActionResult
	if err := proto.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (s *LocalStore) UpdateActionResult(ctx context.Context, digest Digest, result *repb.ActionResult) error {
	data, err := proto.Marshal(result)
	if err != nil {
		return err
	}

	path := s.getActionPath(digest)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	f, err := os.CreateTemp(dir, "ac-tmp-*")
	if err != nil {
		return err
	}
	tmpPath := f.Name()

	defer func() {
		f.Close()
		os.Remove(tmpPath) // Cleanup if rename didn't happen
	}()

	if _, err := f.Write(data); err != nil {
		return err
	}

	if err := f.Close(); err != nil {
		return err
	}

	return os.Rename(tmpPath, path)
}

func (s *LocalStore) Put(ctx context.Context, digest Digest, data io.Reader) error {
	path, _ := s.BlobPath(digest)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		s.logger.Error("failed to create directory", "path", dir, "error", err)
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Use CreateTemp to avoid collisions and the truncation bug
	f, err := os.CreateTemp(dir, "cas-tmp-*")
	if err != nil {
		s.logger.Error("failed to create temp file", "dir", dir, "error", err)
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := f.Name()

	defer func() {
		f.Close()
		os.Remove(tmpPath) // Cleanup if rename didn't happen
	}()

	n, err := io.Copy(f, data)
	if err != nil {
		s.logger.Error("failed to write data", "hash", digest.Hash, "size", digest.Size, "written", n, "error", err)
		return fmt.Errorf("failed to write data: %w", err)
	}
	if n != digest.Size {
		s.logger.Error("digest size mismatch", "hash", digest.Hash, "expected", digest.Size, "got", n)
		return fmt.Errorf("digest size mismatch: expected %d, got %d", digest.Size, n)
	}

	if err := f.Close(); err != nil {
		s.logger.Error("failed to close temp file", "path", tmpPath, "error", err)
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		s.logger.Error("failed to rename temp file", "from", tmpPath, "to", path, "error", err)
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	if err := os.Chmod(path, 0444); err != nil {
		s.logger.Error("failed to make file read-only", "path", path, "error", err)
		return fmt.Errorf("failed to make file read-only: %w", err)
	}

	// Notify callback that a new file was added
	if s.onPutCallback != nil {
		s.onPutCallback()
	}

	return nil
}

func (s *LocalStore) PutFile(ctx context.Context, digest Digest, sourcePath string) error {
	path, _ := s.BlobPath(digest)

	// Check if already exists
	if _, err := os.Stat(path); err == nil {
		if s.forceUpdateATime {
			now := time.Now()
			_ = os.Chtimes(path, now, now)
		}
		return nil
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Create a unique temporary filename manually since we need to use it for hardlinking
	// and Os.CreateTemp creates the file which conflicts with os.Link.
	tmpPath := filepath.Join(dir, fmt.Sprintf("cas-tmp-putfile-%d-%s", time.Now().UnixNano(), digest.Hash))

	// Ensure the temporary file is removed on error before it's renamed.
	defer func() {
		// Only remove if it still exists (i.e., rename didn't happen, or an error occurred post-rename)
		if _, statErr := os.Stat(tmpPath); statErr == nil {
			os.Remove(tmpPath)
		}
	}()

	// Try hard linking first
	linkErr := os.Link(sourcePath, tmpPath)
	if linkErr == nil {
		// Hardlink succeeded, ensure permissions are correct (read-only for CAS)
		// os.Link preserves permissions of source. We want 0444 for cached files.
		if err := os.Chmod(tmpPath, 0444); err != nil {
			s.logger.Error("failed to set read-only permissions on hardlinked file", "path", tmpPath, "error", err)
			return fmt.Errorf("failed to set read-only permissions on hardlinked file: %w", err)
		}
	} else {
		s.logger.Warn("hardlink failed, falling back to copy", "src", sourcePath, "dst", tmpPath, "error", linkErr)

		// Fallback to copy
		src, err := os.Open(sourcePath)
		if err != nil {
			return err
		}
		defer src.Close() // Close source file

		dst, err := os.Create(tmpPath)
		if err != nil {
			return err
		}
		defer dst.Close() // Close destination file (important before rename)

		if _, err := io.Copy(dst, src); err != nil {
			return err
		}
		// Explicitly close dst to ensure all data is flushed before chmod/rename
		if err := dst.Close(); err != nil {
			return err
		}

		if err := os.Chmod(tmpPath, 0444); err != nil {
			s.logger.Error("failed to set read-only permissions on copied file", "path", tmpPath, "error", err)
			return fmt.Errorf("failed to set read-only permissions on copied file: %w", err)
		}
	}

	// Atomically move the temporary file to its final destination
	if err := os.Rename(tmpPath, path); err != nil {
		s.logger.Error("failed to rename temp file to final path", "from", tmpPath, "to", path, "error", err)
		return fmt.Errorf("failed to rename temp file to final path: %w", err)
	}

	// No need for a final Chmod on `path` because `rename` preserves permissions,
	// and we've already set `tmpPath` to 0444.

	// Notify callback that a new file was added
	if s.onPutCallback != nil {
		s.onPutCallback()
	}

	return nil
}
