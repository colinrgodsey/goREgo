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

type LocalStore struct {
	rootDir          string
	forceUpdateATime bool
	logger           *slog.Logger
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

func (s *LocalStore) getBlobPath(digest Digest) string {
	// data/cas/{ab}/{cd}/{digest}
	return filepath.Join(
		s.rootDir,
		"cas",
		digest.Hash[0:2],
		digest.Hash[2:4],
		digest.Hash,
	)
}

func (s *LocalStore) Has(ctx context.Context, digest Digest) (bool, error) {
	path := s.getBlobPath(digest)
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
	path := s.getBlobPath(digest)
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
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {

		return err

	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {

		return err

	}

	return os.Rename(tmpPath, path)
}

func (s *LocalStore) Put(ctx context.Context, digest Digest, data io.Reader) error {
	path := s.getBlobPath(digest)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		s.logger.Error("failed to create directory", "path", filepath.Dir(path), "error", err)
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Write to temp file first for atomicity
	tmpPath := path + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		s.logger.Error("failed to create temp file", "path", tmpPath, "error", err)
		return fmt.Errorf("failed to create temp file: %w", err)
	}

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

	return nil
}
