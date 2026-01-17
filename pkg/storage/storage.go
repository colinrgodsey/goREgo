package storage

import (
	"context"
	"io"

	"github.com/bazelbuild/remote-apis-sdks/go/pkg/digest"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
)

// Digest is a wrapper around the SDK Digest.
type Digest = digest.Digest

func FromProto(d *repb.Digest) (Digest, error) {
	return digest.NewFromProto(d)
}

// BlobStore defines the interface for Content Addressable Storage (CAS).
type BlobStore interface {
	Has(ctx context.Context, digest Digest) (bool, error)
	Get(ctx context.Context, digest Digest) (io.ReadCloser, error)
	Put(ctx context.Context, digest Digest, data io.Reader) error
}

// LocalBlobStore is an optional interface for BlobStores that are backed by a local filesystem.
type LocalBlobStore interface {
	BlobStore
	BlobPath(digest Digest) (string, error)
}

// ActionCache defines the interface for the Action Cache (AC).
type ActionCache interface {
	GetActionResult(ctx context.Context, digest Digest) (*repb.ActionResult, error)
	UpdateActionResult(ctx context.Context, digest Digest, result *repb.ActionResult) error
}
