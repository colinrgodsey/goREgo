package storage

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bazelbuild/remote-apis-sdks/go/pkg/client"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/colinrgodsey/goREgo/pkg/config"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type RemoteStore struct {
	c           *client.Client
	compression string
}

func NewRemoteStore(ctx context.Context, cfg config.BackingCacheConfig) (*RemoteStore, error) {
	dialAddr := strings.TrimPrefix(cfg.Target, "grpc://")
	dialAddr = strings.TrimPrefix(dialAddr, "grpcs://")

	dialParams := client.DialParams{
		Service: dialAddr,
	}

	if cfg.TLS.Enabled {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: false, // Default to secure
		}

		// Load client cert if provided
		if cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "" {
			cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
			if err != nil {
				return nil, fmt.Errorf("failed to load client cert/key: %w", err)
			}
			tlsConfig.Certificates = []tls.Certificate{cert}
		}

		// Load CA if provided
		if cfg.TLS.CAFile != "" {
			caCert, err := os.ReadFile(cfg.TLS.CAFile)
			if err != nil {
				return nil, fmt.Errorf("failed to read CA file: %w", err)
			}
			caCertPool := x509.NewCertPool()
			if !caCertPool.AppendCertsFromPEM(caCert) {
				return nil, fmt.Errorf("failed to append CA cert")
			}
			tlsConfig.RootCAs = caCertPool
		}

		creds := credentials.NewTLS(tlsConfig)
		dialParams.DialOpts = append(dialParams.DialOpts, grpc.WithTransportCredentials(creds))
	} else {
		dialParams.NoSecurity = true
	}

	// Additional dial options (e.g. for larger message sizes if needed)
	dialParams.DialOpts = append(dialParams.DialOpts, grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(100*1024*1024))) // 100MB

	c, err := client.NewClient(ctx, "", dialParams)

	if err != nil {
		return nil, err
	}
	return &RemoteStore{
		c:           c,
		compression: cfg.Compression,
	}, nil
}

func (s *RemoteStore) Close() error {
	return s.c.Close()
}

func (s *RemoteStore) Has(ctx context.Context, digest Digest) (bool, error) {
	missing, err := s.c.MissingBlobs(ctx, []Digest{digest})
	if err != nil {
		return false, err
	}
	return len(missing) == 0, nil
}

type readCloserWrapper struct {
	io.Reader
	io.Closer
}

type cancelCloser struct {
	io.Closer
	cancel context.CancelFunc
}

func (c *cancelCloser) Close() error {
	c.cancel()
	return c.Closer.Close()
}

func (s *RemoteStore) Get(ctx context.Context, digest Digest) (io.ReadCloser, error) {
	var resourceName string
	var err error

	if s.compression == "zstd" {
		resourceName, err = s.c.ResourceName("compressed-blobs/zstd", digest.Hash, fmt.Sprintf("%d", digest.Size))
	} else {
		resourceName, err = s.c.ResourceName("blobs", digest.Hash, fmt.Sprintf("%d", digest.Size))
	}
	if err != nil {
		return nil, err
	}

	// Create a cancelable context to ensure stream closes if reader is closed
	ctx, cancel := context.WithCancel(ctx)
	stream, err := s.c.Read(ctx, &bytestream.ReadRequest{
		ResourceName: resourceName,
	})
	if err != nil {
		cancel()
		return nil, err
	}

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if _, err := pw.Write(resp.Data); err != nil {
				// Reader closed or error
				return
			}
		}
	}()

	closer := &cancelCloser{Closer: pr, cancel: cancel}

	if s.compression == "zstd" {
		decoder, err := zstd.NewReader(pr)
		if err != nil {
			closer.Close()
			return nil, err
		}
		return &readCloserWrapper{Reader: decoder, Closer: closer}, nil
	}

	return &readCloserWrapper{Reader: pr, Closer: closer}, nil
}

func (s *RemoteStore) Put(ctx context.Context, digest Digest, data io.Reader) error {
	var resourceName string
	var err error

	if s.compression == "zstd" {
		// Use manual resource name construction for compressed uploads
		prefix := "uploads/" + uuid.New().String() + "/compressed-blobs/zstd"
		resourceName, err = s.c.ResourceName(prefix, digest.Hash, fmt.Sprintf("%d", digest.Size))
	} else {
		resourceName, err = s.c.ResourceNameWrite(digest.Hash, digest.Size)
	}
	if err != nil {
		return err
	}

	stream, err := s.c.Write(ctx)
	if err != nil {
		return err
	}

	// Prepare data stream
	var readerToConsume io.Reader = data

	if s.compression == "zstd" {
		pr, pw := io.Pipe()
		defer pr.Close()

		// Run compressor in background
		go func() {
			enc, _ := zstd.NewWriter(pw)
			if _, err := io.Copy(enc, data); err != nil {
				pw.CloseWithError(err)
				return
			}
			if err := enc.Close(); err != nil {
				pw.CloseWithError(err)
				return
			}
			pw.Close()
		}()

		readerToConsume = pr
	}

	buf := make([]byte, 32*1024)
	var offset int64
	for {
		n, readErr := readerToConsume.Read(buf)
		if n > 0 {
			req := &bytestream.WriteRequest{
				ResourceName: resourceName,
				WriteOffset:  offset,
				Data:         buf[:n],
			}
			if offset > 0 {
				req.ResourceName = ""
			}

			if err := stream.Send(req); err != nil {
				// Server may have closed the stream early (blob already exists).
				// Try to get the response to check if it was a successful commit.
				if resp, recvErr := stream.CloseAndRecv(); recvErr == nil {
					if s.isCommitSuccessful(resp, digest) {
						return nil
					}
				}
				return err
			}
			offset += int64(n)
		}

		if readErr == io.EOF {
			req := &bytestream.WriteRequest{
				WriteOffset: offset,
				FinishWrite: true,
			}
			if offset == 0 {
				req.ResourceName = resourceName
			} else {
				req.ResourceName = ""
			}

			if err := stream.Send(req); err != nil {
				// Same early-close handling for the final send
				if resp, recvErr := stream.CloseAndRecv(); recvErr == nil {
					if s.isCommitSuccessful(resp, digest) {
						return nil
					}
				}
				return err
			}
			resp, err := stream.CloseAndRecv()
			if err != nil {
				return err
			}
			// Verify the server committed the expected size
			if !s.isCommitSuccessful(resp, digest) {
				return fmt.Errorf("incomplete write: committed %d, expected %d", resp.CommittedSize, digest.Size)
			}
			return nil
		}

		if readErr != nil {
			return readErr
		}
	}
}

// isCommitSuccessful checks if the WriteResponse indicates a successful commit.
// For compressed uploads, CommittedSize varies by server implementation:
//   - bazel-remote returns -1 for skipped writes (blob exists), or uncompressed size
//   - Other servers may return the compressed size
//
// For uncompressed uploads, CommittedSize should equal the digest size.
func (s *RemoteStore) isCommitSuccessful(resp *bytestream.WriteResponse, digest Digest) bool {
	if s.compression == "zstd" {
		// Compressed: accept any non-zero commit (compressed size varies)
		// -1 means blob already existed (skipped write)
		// >0 means data was written (could be compressed or uncompressed size)
		return resp.CommittedSize != 0
	}
	return resp.CommittedSize == digest.Size
}

// ActionCache implementation

func (s *RemoteStore) GetActionResult(ctx context.Context, digest Digest) (*repb.ActionResult, error) {
	return s.c.GetActionResult(ctx, &repb.GetActionResultRequest{
		InstanceName: s.c.InstanceName,
		ActionDigest: digest.ToProto(),
	})
}

func (s *RemoteStore) UpdateActionResult(ctx context.Context, digest Digest, result *repb.ActionResult) error {
	_, err := s.c.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{
		InstanceName: s.c.InstanceName,
		ActionDigest: digest.ToProto(),
		ActionResult: result,
	})
	return err
}
