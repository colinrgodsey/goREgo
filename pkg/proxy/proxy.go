package proxy

import (
	"context"
	"io"
	"log/slog"
	"os"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/colinrgodsey/goREgo/pkg/storage"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/singleflight"
)

type ProxyStore struct {
	local     *storage.LocalStore
	remote    storage.BlobStore // Tier 2
	localOnly bool              // true when remote == local (no backing cache)
	group     singleflight.Group
	tracer    trace.Tracer
	logger    *slog.Logger
}

func NewProxyStore(local *storage.LocalStore, remote storage.BlobStore) *ProxyStore {
	// Detect if we're in local-only mode (no backing cache)
	localOnly := false
	if remote == nil {
		remote = local
		localOnly = true
	} else if l, ok := remote.(*storage.LocalStore); ok && l == local {
		localOnly = true
	}

	return &ProxyStore{
		local:     local,
		remote:    remote,
		localOnly: localOnly,
		tracer:    otel.Tracer("gorego/pkg/proxy"),
		logger:    slog.Default().With("component", "proxy"),
	}
}

func (p *ProxyStore) Has(ctx context.Context, digest storage.Digest) (bool, error) {
	ctx, span := p.tracer.Start(ctx, "proxy.Has", trace.WithAttributes(
		attribute.String("digest.hash", digest.Hash),
		attribute.Int64("digest.size", digest.Size),
	))
	defer span.End()

	ok, err := p.local.Has(ctx, digest)
	if err != nil {
		span.RecordError(err)
		return false, err
	}
	if ok {
		span.SetAttributes(attribute.Bool("cache.hit", true))
		return true, nil
	}

	// Local-only mode: no remote to check
	if p.localOnly {
		span.SetAttributes(attribute.Bool("cache.hit", false))
		return false, nil
	}

	// Check remote for FindMissingBlobs
	found, err := p.remote.Has(ctx, digest)
	span.SetAttributes(attribute.Bool("cache.hit", false), attribute.Bool("remote.found", found))
	if err != nil {
		span.RecordError(err)
	}
	return found, err
}

func (p *ProxyStore) Get(ctx context.Context, digest storage.Digest) (io.ReadCloser, error) {
	ctx, span := p.tracer.Start(ctx, "proxy.Get", trace.WithAttributes(
		attribute.String("digest.hash", digest.Hash),
		attribute.Int64("digest.size", digest.Size),
	))
	defer span.End()

	// 1. Check local
	if ok, _ := p.local.Has(ctx, digest); ok {
		span.SetAttributes(attribute.Bool("cache.hit", true))
		return p.local.Get(ctx, digest)
	}
	span.SetAttributes(attribute.Bool("cache.hit", false))

	// Local-only mode: blob not found
	if p.localOnly {
		return nil, os.ErrNotExist
	}

	// 2. Singleflight fetch from remote
	_, err, _ := p.group.Do(digest.Hash, func() (interface{}, error) {
		ctx, span := p.tracer.Start(ctx, "proxy.Get.RemoteFetch")
		defer span.End()

		rc, err := p.remote.Get(ctx, digest)
		if err != nil {
			span.RecordError(err)
			return nil, err
		}
		defer rc.Close()

		// Stream to local
		if err := p.local.Put(ctx, digest, rc); err != nil {
			span.RecordError(err)
			return nil, err
		}
		return nil, nil
	})

	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	// 3. Return local handle
	return p.local.Get(ctx, digest)
}

func (p *ProxyStore) Put(ctx context.Context, digest storage.Digest, data io.Reader) error {
	ctx, span := p.tracer.Start(ctx, "proxy.Put", trace.WithAttributes(
		attribute.String("digest.hash", digest.Hash),
		attribute.Int64("digest.size", digest.Size),
		attribute.Bool("local_only", p.localOnly),
	))
	defer span.End()

	p.logger.Debug("put starting", "hash", digest.Hash, "size", digest.Size, "local_only", p.localOnly)

	// Local-only mode: use TeeReader with io.Discard as null consumer
	// This ensures the tee logic is always exercised for consistent behavior
	if p.localOnly {
		tee := io.TeeReader(data, io.Discard)
		if err := p.local.Put(ctx, digest, tee); err != nil {
			p.logger.Error("local put failed", "hash", digest.Hash, "size", digest.Size, "error", err)
			span.RecordError(err)
			return err
		}
		p.logger.Debug("put complete", "hash", digest.Hash, "size", digest.Size)
		return nil
	}

	// Write-through: Tee data to both local and remote.
	// We rely on "Hard Dependency" logic: if either fails, the operation fails.
	// Since remote is authoritative, we stream to it concurrently.

	pr, pw := io.Pipe()
	remoteErrChan := make(chan error, 1)

	go func() {
		// Remote consumes the pipe
		err := p.remote.Put(ctx, digest, pr)
		// If remote fails early, close the reader to signal the writer (local put)
		// If success, this is a no-op as writer already closed
		_ = pr.CloseWithError(err)
		remoteErrChan <- err
	}()

	// Tee data: Read from 'data', write to 'pw' (which goes to remote), return bytes to 'local'
	tee := io.TeeReader(data, pw)

	// Local consumes the TeeReader
	localErr := p.local.Put(ctx, digest, tee)

	// Close the writer end to signal EOF to remote
	if localErr != nil {
		_ = pw.CloseWithError(localErr)
	} else {
		_ = pw.Close()
	}

	// Wait for remote to finish
	remoteErr := <-remoteErrChan

	if localErr != nil {
		p.logger.Error("local put failed", "hash", digest.Hash, "size", digest.Size, "error", localErr)
		span.RecordError(localErr)
		return localErr
	}
	if remoteErr != nil {
		p.logger.Error("remote put failed", "hash", digest.Hash, "size", digest.Size, "error", remoteErr)
		span.RecordError(remoteErr)
		return remoteErr
	}

	p.logger.Debug("put complete", "hash", digest.Hash, "size", digest.Size)
	return nil
}

// Implement ActionCache interface...
func (p *ProxyStore) GetActionResult(ctx context.Context, digest storage.Digest) (*repb.ActionResult, error) {
	ctx, span := p.tracer.Start(ctx, "proxy.GetActionResult", trace.WithAttributes(
		attribute.String("digest.hash", digest.Hash),
	))
	defer span.End()

	// Read-through
	res, err := p.local.GetActionResult(ctx, digest)
	if err == nil {
		span.SetAttributes(attribute.Bool("cache.hit", true))
		return res, nil
	}
	span.SetAttributes(attribute.Bool("cache.hit", false))

	// Fetch from remote
	val, err, _ := p.group.Do("ac:"+digest.Hash, func() (interface{}, error) {
		ctx, span := p.tracer.Start(ctx, "proxy.GetActionResult.RemoteFetch")
		defer span.End()

		res, err := p.remote.(storage.ActionCache).GetActionResult(ctx, digest)
		if err != nil {
			span.RecordError(err)
			return nil, err
		}

		// Cache locally
		_ = p.local.UpdateActionResult(ctx, digest, res)
		return res, nil
	})

	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	return val.(*repb.ActionResult), nil
}

func (p *ProxyStore) UpdateActionResult(ctx context.Context, digest storage.Digest, result *repb.ActionResult) error {
	ctx, span := p.tracer.Start(ctx, "proxy.UpdateActionResult", trace.WithAttributes(
		attribute.String("digest.hash", digest.Hash),
	))
	defer span.End()

	// Write-through
	if err := p.remote.(storage.ActionCache).UpdateActionResult(ctx, digest, result); err != nil {
		span.RecordError(err)
		return err
	}
	return p.local.UpdateActionResult(ctx, digest, result)
}
