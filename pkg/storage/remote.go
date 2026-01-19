package storage

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/bazelbuild/remote-apis-sdks/go/pkg/client"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/genproto/googleapis/bytestream"
)

type RemoteStore struct {
	c           *client.Client
	compression string
}

func NewRemoteStore(ctx context.Context, target string, compression string) (*RemoteStore, error) {
	// TODO: Support TLS/Auth configuration
	dialAddr := strings.TrimPrefix(target, "grpc://")
	c, err := client.NewClient(ctx, "", client.DialParams{
		Service:    dialAddr,
		NoSecurity: true,
	})
	if err != nil {
		return nil, err
	}
	return &RemoteStore{
		c:           c,
		compression: compression,
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

	stream, err := s.c.Read(ctx, &bytestream.ReadRequest{
		ResourceName: resourceName,
	})
	if err != nil {
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

	if s.compression == "zstd" {
		decoder, err := zstd.NewReader(pr)
		if err != nil {
			pr.Close()
			return nil, err
		}
		return &readCloserWrapper{Reader: decoder, Closer: pr}, nil
	}

	return pr, nil
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
				return err
			}
			_, err := stream.CloseAndRecv()
			return err
		}

		if readErr != nil {
			return readErr
		}
	}
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
