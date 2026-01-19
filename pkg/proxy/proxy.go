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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type ProxyStore struct {
	local         *storage.LocalStore
	remote        storage.BlobStore // Tier 2
	group         singleflight.Group
	tracer        trace.Tracer
	logger        *slog.Logger
	putRetryCount int
}

func NewProxyStore(local *storage.LocalStore, remote storage.BlobStore, putRetryCount int) *ProxyStore {
	if putRetryCount < 1 {
		putRetryCount = 1 // At least one attempt
	}
	return &ProxyStore{
		local:         local,
		remote:        remote,
		tracer:        otel.Tracer("gorego/pkg/proxy"),
		logger:        slog.Default().With("component", "proxy"),
		putRetryCount: putRetryCount,
	}
}

func (p *ProxyStore) BlobPath(digest storage.Digest) (string, error) {
	return p.local.BlobPath(digest)
}

func (p *ProxyStore) PutFile(ctx context.Context, digest storage.Digest, path string) error {
	ctx, span := p.tracer.Start(ctx, "proxy.PutFile", trace.WithAttributes(
		attribute.String("digest.hash", digest.Hash),
		attribute.Int64("digest.size", digest.Size),
	))
	defer span.End()

	// 1. Put to local (hardlink optimized)
	if err := p.local.PutFile(ctx, digest, path); err != nil {
		span.RecordError(err)
		return err
	}

	// 2. Upload to remote with retry logic
	var lastErr error
	for attempt := 1; attempt <= p.putRetryCount; attempt++ {
		f, err := os.Open(path)
		if err != nil {
			span.RecordError(err)
			return err
		}

		err = p.remote.Put(ctx, digest, f)
		f.Close()

		if err == nil {
			if attempt > 1 {
				p.logger.Info("remote put file succeeded after retry",
					"hash", digest.Hash, "attempt", attempt)
			}
			return nil
		}

		lastErr = err
		p.logger.Warn("remote put file failed",
			"hash", digest.Hash, "attempt", attempt, "max_attempts", p.putRetryCount, "error", err)
		span.RecordError(err)

		// Check if context is cancelled before retrying
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	p.logger.Error("remote put file failed after all retries",
		"hash", digest.Hash, "attempts", p.putRetryCount, "error", lastErr)
	return lastErr
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

	p.logger.Debug("put starting", "hash", digest.Hash, "size", digest.Size)

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

		// If remote is NullStore (which doesn't implement ActionCache explicitly in the struct definition here but does in reality),
		// we need to be careful. storage.BlobStore doesn't include ActionCache methods.
		// However, NewProxyStore takes BlobStore. The previous code casted p.remote to ActionCache.
		// We assume remote ALWAYS implements ActionCache (RemoteStore does, NullStore does).
		if ac, ok := p.remote.(storage.ActionCache); ok {
			res, err := ac.GetActionResult(ctx, digest)
			if err != nil {
				span.RecordError(err)
				return nil, err
			}
			// Cache locally
			_ = p.local.UpdateActionResult(ctx, digest, res)
			return res, nil
		}
		// If remote doesn't support ActionCache, we can't fetch.
		return nil, status.Error(codes.Unimplemented, "remote does not support ActionCache")
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
	if ac, ok := p.remote.(storage.ActionCache); ok {
		if err := ac.UpdateActionResult(ctx, digest, result); err != nil {
			span.RecordError(err)
			return err
		}
	} else {
		// If remote is not an ActionCache, strictly speaking we might want to error or just skip.
		// Given we want unified behavior, and NullStore implements it, we should generally expect it to exist.
		// But if someone passes a simple BlobStore mock, this prevents panic.
	}
	return p.local.UpdateActionResult(ctx, digest, result)
}
