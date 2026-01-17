package server

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/colinrgodsey/goREgo/pkg/storage"
	"google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MockBlobStore implements storage.BlobStore
type MockBlobStore struct {
	putFunc func(ctx context.Context, digest storage.Digest, data io.Reader) error
}

func (m *MockBlobStore) Has(ctx context.Context, digest storage.Digest) (bool, error) {
	return false, nil
}

func (m *MockBlobStore) Get(ctx context.Context, digest storage.Digest) (io.ReadCloser, error) {
	return nil, nil
}

func (m *MockBlobStore) Put(ctx context.Context, digest storage.Digest, data io.Reader) error {
	if m.putFunc != nil {
		return m.putFunc(ctx, digest, data)
	}
	return nil
}

// mockWriteServer implements bytestream.ByteStream_WriteServer
type mockWriteServer struct {
	grpc.ServerStream
	ctx      context.Context
	recvFunc func() (*bytestream.WriteRequest, error)
	sentResp *bytestream.WriteResponse
}

func (m *mockWriteServer) Context() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

func (m *mockWriteServer) Recv() (*bytestream.WriteRequest, error) {
	return m.recvFunc()
}

func (m *mockWriteServer) SendAndClose(resp *bytestream.WriteResponse) error {
	m.sentResp = resp
	return nil
}

func TestByteStreamServer_Write_OffsetValidation(t *testing.T) {
	store := &MockBlobStore{}
	server := NewByteStreamServer(store)

	reqs := []*bytestream.WriteRequest{
		{
			ResourceName: "uploads/uuid/blobs/abc/123",
			WriteOffset:  10, // Non-zero offset
			Data:         []byte("test"),
		},
	}
	reqIndex := 0

	stream := &mockWriteServer{
		recvFunc: func() (*bytestream.WriteRequest, error) {
			if reqIndex >= len(reqs) {
				return nil, io.EOF
			}
			req := reqs[reqIndex]
			reqIndex++
			return req, nil
		},
	}

	err := server.Write(stream)
	if err == nil {
		t.Error("Expected error for non-zero offset, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Errorf("Expected gRPC status error, got %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("Expected InvalidArgument code, got %v", st.Code())
	}
}

func TestByteStreamServer_Write_Success(t *testing.T) {
	store := &MockBlobStore{
		putFunc: func(ctx context.Context, digest storage.Digest, data io.Reader) error {
			buf := new(bytes.Buffer)
			_, err := io.Copy(buf, data)
			if err != nil {
				return err
			}
			if buf.String() != "hello" {
				t.Errorf("Expected 'hello', got %q", buf.String())
			}
			return nil
		},
	}
	server := NewByteStreamServer(store)

	reqs := []*bytestream.WriteRequest{
		{
			ResourceName: "uploads/uuid/blobs/abc/5",
			WriteOffset:  0,
			Data:         []byte("hello"),
			FinishWrite:  true,
		},
	}
	reqIndex := 0

	stream := &mockWriteServer{
		recvFunc: func() (*bytestream.WriteRequest, error) {
			if reqIndex >= len(reqs) {
				return nil, io.EOF
			}
			req := reqs[reqIndex]
			reqIndex++
			return req, nil
		},
	}

	err := server.Write(stream)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
}
