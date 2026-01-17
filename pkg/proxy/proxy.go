package proxy

import (
	"context"
	"io"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/colinrgodsey/goREgo/pkg/storage"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/singleflight"
)

type ProxyStore struct {
	local  *storage.LocalStore
	remote storage.BlobStore // Tier 2
	group  singleflight.Group
	tracer trace.Tracer
}

func NewProxyStore(local *storage.LocalStore, remote storage.BlobStore) *ProxyStore {
	return &ProxyStore{
		local:  local,
		remote: remote,
		tracer: otel.Tracer("gorego/pkg/proxy"),
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
	// Check remote? Usually Has() is for FindMissingBlobs.
	// We should check remote if local misses.
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
	))
	defer span.End()

	// Write-through: Write to remote (authoritative), then local.

	// We need to read the data twice? Or tee it?
	// If we write to remote, we might consume the reader.
	// Ideally, we write to a temp file locally, then upload to remote, then commit local?

	// For now, simple approach: Write to local temp, then upload.
	// But `storage.Put` takes a Reader.

	// Implementation note: The `data` reader is usually a stream from the client.
	// If we fail to upload to Tier 2, we must fail the request.

	// We can use `p.local.Put` first (which writes to disk), then read it back to upload to remote?
	// This ensures we have the data.

	if err := p.local.Put(ctx, digest, data); err != nil {
		span.RecordError(err)
		return err
	}

	// Read back
	rc, err := p.local.Get(ctx, digest)
	if err != nil {
		span.RecordError(err)
		return err
	}
	defer rc.Close()

	if err := p.remote.Put(ctx, digest, rc); err != nil {
		span.RecordError(err)
		// If remote fails, we technically "have" it locally, but we violate "Tier 2 authoritative".
		// We should probably delete local?
		// For now, just return error.
		return err
	}

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
	// TODO: Add singleflight here too
	res, err = p.remote.(storage.ActionCache).GetActionResult(ctx, digest)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	// Cache locally
	_ = p.local.UpdateActionResult(ctx, digest, res)
	return res, nil
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
