package storage

import (
	"context"
	"fmt"
	"io"

	"github.com/bazelbuild/remote-apis-sdks/go/pkg/client"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/genproto/googleapis/bytestream"
)

type RemoteStore struct {
	c *client.Client
}

func NewRemoteStore(ctx context.Context, target string) (*RemoteStore, error) {
	// TODO: Support TLS/Auth configuration
	c, err := client.NewClient(ctx, "", client.DialParams{
		Service:    target,
		NoSecurity: true,
	})
	if err != nil {
		return nil, err
	}
	return &RemoteStore{c: c}, nil
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

func (s *RemoteStore) Get(ctx context.Context, digest Digest) (io.ReadCloser, error) {
	resourceName, err := s.c.ResourceName("blobs", digest.Hash, fmt.Sprintf("%d", digest.Size))
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

	return pr, nil
}

func (s *RemoteStore) Put(ctx context.Context, digest Digest, data io.Reader) error {
	// If we use c.Write(ctx), it returns a client.
	// But we need to handle the resource name and uploading.
	// The SDK client has helper methods but mostly for []byte or file paths.

	// We'll use the raw ByteStream Write.
	resourceName, err := s.c.ResourceNameWrite(digest.Hash, digest.Size)
	if err != nil {
		return err
	}

	stream, err := s.c.Write(ctx)
	if err != nil {
		return err
	}

	buf := make([]byte, 32*1024)
	var offset int64
	for {
		n, readErr := data.Read(buf)
		if n > 0 {
			req := &bytestream.WriteRequest{
				ResourceName: resourceName,
				WriteOffset:  offset,
				Data:         buf[:n],
			}
			// Only send ResourceName on first message?
			// The proto says: "The first request of a Write operation must contain a non-empty resource_name."
			// Subsequent ones can leave it empty.
			if offset > 0 {
				req.ResourceName = ""
			}

			if err := stream.Send(req); err != nil {
				return err
			}
			offset += int64(n)
		}

		if readErr == io.EOF {
			// Finish write
			req := &bytestream.WriteRequest{
				WriteOffset: offset,
				FinishWrite: true,
			}
			// If we haven't sent anything yet (empty file), we need resource name.
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
