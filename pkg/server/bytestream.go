package server

import (
	"context"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/colinrgodsey/goREgo/pkg/storage"
	"github.com/klauspost/compress/zstd"
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

// Support both 'blobs' and 'compressed-blobs/zstd'
var (
	readResourceRegex  = regexp.MustCompile(`(?:^|.*/)(blobs|compressed-blobs/zstd)/([a-fA-F0-9]+)/(\d+)$`)
	writeResourceRegex = regexp.MustCompile(`(?:^|.*/)uploads/[^/]+/(blobs|compressed-blobs/zstd)/([a-fA-F0-9]+)/(\d+)$`)
)

func parseResourceName(name string, isWrite bool) (storage.Digest, bool, error) {
	re := readResourceRegex
	if isWrite {
		re = writeResourceRegex
	}

	matches := re.FindStringSubmatch(name)
	if len(matches) != 4 {
		return storage.Digest{}, false, status.Errorf(codes.InvalidArgument, "invalid resource name: %s", name)
	}

	typeStr := matches[1]
	hash := matches[2]
	size, err := strconv.ParseInt(matches[3], 10, 64)
	if err != nil {
		return storage.Digest{}, false, status.Errorf(codes.InvalidArgument, "invalid size in resource name: %v", err)
	}

	isCompressed := strings.HasPrefix(typeStr, "compressed-blobs")

	return storage.Digest{
		Hash: hash,
		Size: size,
	}, isCompressed, nil
}

type readCloserWrapper struct {
	io.Reader
	io.Closer
}

func (s *ByteStreamServer) Read(req *bytestream.ReadRequest, stream bytestream.ByteStream_ReadServer) error {
	dg, isCompressed, err := parseResourceName(req.ResourceName, false)
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

	var reader io.Reader = rc
	if isCompressed {
		// Compress on the fly
		pr, pw := io.Pipe()
		go func() {
			enc, _ := zstd.NewWriter(pw)
			if _, err := io.Copy(enc, rc); err != nil {
				pw.CloseWithError(err)
				return
			}
			if err := enc.Close(); err != nil {
				pw.CloseWithError(err)
				return
			}
			pw.Close()
		}()
		reader = pr
		defer pr.Close() // Ensure pipe reader is closed if we exit early
	}

	if req.ReadOffset > 0 {
		if isCompressed {
			// Cannot seek in compressed stream easily
			if _, err := io.CopyN(io.Discard, reader, req.ReadOffset); err != nil {
				return status.Errorf(codes.Internal, "failed to skip offset in compressed stream: %v", err)
			}
		} else {
			if seeker, ok := rc.(io.Seeker); ok {
				if _, err := seeker.Seek(req.ReadOffset, io.SeekStart); err != nil {
					return status.Errorf(codes.Internal, "failed to seek: %v", err)
				}
			} else {
				if _, err := io.CopyN(io.Discard, rc, req.ReadOffset); err != nil {
					return status.Errorf(codes.Internal, "failed to skip offset: %v", err)
				}
			}
		}
	}

	buf := make([]byte, readBufferSize)
	var totalSent int64
	// If limit is set, use it. Else assume infinite.
	limit := int64(1<<63 - 1)
	if req.ReadLimit > 0 {
		limit = req.ReadLimit
	}

	for totalSent < limit {
		toRead := int64(len(buf))
		if limit-totalSent < toRead {
			toRead = limit - totalSent
		}

		n, err := reader.Read(buf[:toRead])
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
	var isCompressed bool
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
			dg, isCompressed, err = parseResourceName(req.ResourceName, true)
			if err != nil {
				s.logger.Error("failed to parse resource name for write", "resource", req.ResourceName, "error", err)
				return err
			}
			initialized = true
			s.logger.Debug("starting write", "hash", dg.Hash, "size", dg.Size, "compressed", isCompressed)

			// Start the Put operation in background
			go func() {
				var input io.Reader = pr
				if isCompressed {
					decoder, err := zstd.NewReader(pr)
					if err != nil {
						errChan <- err
						return
					}
					input = decoder
					// Ensure decoder is closed if it implements Closer (zstd.Decoder has Close, but NewReader returns *Decoder which implements Read, but we need to check docs/implementation)
					defer decoder.Close()
				}
				errChan <- s.Store.Put(stream.Context(), dg, input)
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
