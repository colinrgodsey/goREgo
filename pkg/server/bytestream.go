package server

import (
	"context"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strconv"

	"github.com/colinrgodsey/goREgo/pkg/storage"
	"google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const readBufferSize = 64 * 1024

type ByteStreamServer struct {
	bytestream.UnimplementedByteStreamServer
	Store  storage.BlobStore
	logger *slog.Logger
}

func NewByteStreamServer(store storage.BlobStore) *ByteStreamServer {
	return &ByteStreamServer{
		Store:  store,
		logger: slog.Default().With("component", "bytestream"),
	}
}

var (
	readResourceRegex  = regexp.MustCompile(`(?:^|.*/)blobs/([a-fA-F0-9]+)/(\d+)$`)
	writeResourceRegex = regexp.MustCompile(`(?:^|.*/)uploads/[^/]+/blobs/([a-fA-F0-9]+)/(\d+)$`)
)

func parseResourceName(name string, isWrite bool) (storage.Digest, error) {
	re := readResourceRegex
	if isWrite {
		re = writeResourceRegex
	}

	matches := re.FindStringSubmatch(name)
	if len(matches) != 3 {
		return storage.Digest{}, status.Errorf(codes.InvalidArgument, "invalid resource name: %s", name)
	}

	hash := matches[1]
	size, err := strconv.ParseInt(matches[2], 10, 64)
	if err != nil {
		return storage.Digest{}, status.Errorf(codes.InvalidArgument, "invalid size in resource name: %v", err)
	}

	return storage.Digest{
		Hash: hash,
		Size: size,
	}, nil
}

func (s *ByteStreamServer) Read(req *bytestream.ReadRequest, stream bytestream.ByteStream_ReadServer) error {
	dg, err := parseResourceName(req.ResourceName, false)
	if err != nil {
		s.logger.Error("failed to parse resource name for read", "resource", req.ResourceName, "error", err)
		return err
	}

	rc, err := s.Store.Get(stream.Context(), dg)
	if err != nil {
		if os.IsNotExist(err) || status.Code(err) == codes.NotFound {
			s.logger.Debug("blob not found", "hash", dg.Hash, "size", dg.Size)
			return status.Errorf(codes.NotFound, "blob not found: %v", dg)
		}
		s.logger.Error("failed to get blob", "hash", dg.Hash, "size", dg.Size, "error", err)
		return status.Errorf(codes.Internal, "failed to get blob: %v", err)
	}
	defer rc.Close()

	if req.ReadOffset > 0 {
		if seeker, ok := rc.(io.Seeker); ok {
			if _, err := seeker.Seek(req.ReadOffset, io.SeekStart); err != nil {
				return status.Errorf(codes.Internal, "failed to seek: %v", err)
			}
		} else {
			// Fallback if not seekable
			if _, err := io.CopyN(io.Discard, rc, req.ReadOffset); err != nil {
				return status.Errorf(codes.Internal, "failed to skip offset: %v", err)
			}
		}
	}

	limit := dg.Size - req.ReadOffset
	if req.ReadLimit > 0 && req.ReadLimit < limit {
		limit = req.ReadLimit
	}

	buf := make([]byte, readBufferSize)
	var totalSent int64
	for totalSent < limit {
		toRead := int64(len(buf))
		if limit-totalSent < toRead {
			toRead = limit - totalSent
		}

		n, err := rc.Read(buf[:toRead])
		if n > 0 {
			if err := stream.Send(&bytestream.ReadResponse{Data: buf[:n]}); err != nil {
				return err
			}
			totalSent += int64(n)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return status.Errorf(codes.Internal, "read error: %v", err)
		}
	}

	return nil
}

func (s *ByteStreamServer) Write(stream bytestream.ByteStream_WriteServer) error {
	var dg storage.Digest
	var initialized bool
	var totalWritten int64
	var resourceName string

	pr, pw := io.Pipe()
	errChan := make(chan error, 1)

	defer func() {
		pr.Close()
		pw.Close()
	}()

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.logger.Error("failed to receive write request", "error", err)
			return err
		}

		if !initialized {
			if req.ResourceName == "" {
				s.logger.Error("missing resource name in first message")
				return status.Error(codes.InvalidArgument, "missing resource name in first message")
			}
			if req.WriteOffset > 0 {
				s.logger.Error("resumable uploads not supported", "resource", req.ResourceName, "offset", req.WriteOffset)
				return status.Errorf(codes.InvalidArgument, "resumable uploads not supported (offset %d > 0)", req.WriteOffset)
			}
			resourceName = req.ResourceName
			dg, err = parseResourceName(req.ResourceName, true)
			if err != nil {
				s.logger.Error("failed to parse resource name for write", "resource", req.ResourceName, "error", err)
				return err
			}
			initialized = true
			s.logger.Debug("starting write", "hash", dg.Hash, "size", dg.Size)

			// Start the Put operation in background now that we have the digest
			go func() {
				errChan <- s.Store.Put(stream.Context(), dg, pr)
			}()
		}

		if len(req.Data) > 0 {
			n, err := pw.Write(req.Data)
			if err != nil {
				s.logger.Error("pipe write failed", "hash", dg.Hash, "size", dg.Size, "error", err)
				return status.Errorf(codes.Internal, "pipe write failed: %v", err)
			}
			totalWritten += int64(n)
		}

		if req.FinishWrite {
			break
		}
	}

	pw.Close()

	if !initialized {
		s.logger.Error("never received resource name")
		return status.Error(codes.InvalidArgument, "never received resource name")
	}

	select {
	case err := <-errChan:
		if err != nil {
			s.logger.Error("store put failed", "hash", dg.Hash, "size", dg.Size, "written", totalWritten, "resource", resourceName, "error", err)
			return status.Errorf(codes.Internal, "store put failed: %v", err)
		}
	case <-stream.Context().Done():
		s.logger.Error("context cancelled during write", "hash", dg.Hash, "size", dg.Size, "error", stream.Context().Err())
		return stream.Context().Err()
	}

	s.logger.Debug("write complete", "hash", dg.Hash, "size", dg.Size, "written", totalWritten)
	return stream.SendAndClose(&bytestream.WriteResponse{
		CommittedSize: totalWritten,
	})
}

func (s *ByteStreamServer) QueryWriteStatus(ctx context.Context, req *bytestream.QueryWriteStatusRequest) (*bytestream.QueryWriteStatusResponse, error) {
	return nil, status.Error(codes.Unimplemented, "QueryWriteStatus not implemented")
}
