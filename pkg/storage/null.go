package storage

import (
	"context"
	"io"
	"os"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// NullStore implements BlobStore and ActionCache but does nothing.
// It is used when no remote backing cache is configured.
type NullStore struct{}

func NewNullStore() *NullStore {
	return &NullStore{}
}

// BlobStore implementation

func (s *NullStore) Has(ctx context.Context, digest Digest) (bool, error) {
	return false, nil
}

func (s *NullStore) Get(ctx context.Context, digest Digest) (io.ReadCloser, error) {
	return nil, os.ErrNotExist
}

func (s *NullStore) Put(ctx context.Context, digest Digest, data io.Reader) error {
	// Drain the reader to satisfy the contract (e.g. if it's a pipe)
	_, err := io.Copy(io.Discard, data)
	return err
}

// ActionCache implementation

func (s *NullStore) GetActionResult(ctx context.Context, digest Digest) (*repb.ActionResult, error) {
	return nil, status.Error(codes.NotFound, "not found")
}

func (s *NullStore) UpdateActionResult(ctx context.Context, digest Digest, result *repb.ActionResult) error {
	return nil
}
