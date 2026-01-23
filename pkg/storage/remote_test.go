package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/colinrgodsey/goREgo/pkg/config"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
)

type mockCapabilities struct {
	repb.UnimplementedCapabilitiesServer
}

func (m *mockCapabilities) GetCapabilities(ctx context.Context, req *repb.GetCapabilitiesRequest) (*repb.ServerCapabilities, error) {
	return &repb.ServerCapabilities{
		CacheCapabilities: &repb.CacheCapabilities{
			DigestFunctions: []repb.DigestFunction_Value{repb.DigestFunction_SHA256},
		},
	}, nil
}

type mockByteStream struct {
	bytestream.UnimplementedByteStreamServer
	stored map[string][]byte
}

func (m *mockByteStream) Read(req *bytestream.ReadRequest, stream bytestream.ByteStream_ReadServer) error {
	data, ok := m.stored[req.ResourceName]
	if !ok {
		return fmt.Errorf("not found: %s", req.ResourceName)
	}
	return stream.Send(&bytestream.ReadResponse{Data: data})
}

func (m *mockByteStream) Write(stream bytestream.ByteStream_WriteServer) error {
	var resourceName string
	var buf bytes.Buffer
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			// Simulate commit: convert uploads/uuid/blob -> blob
			// Format: uploads/{uuid}/{type}/{hash}/{size}
			// We want to strip "uploads/{uuid}/"
			if strings.HasPrefix(resourceName, "uploads/") {
				parts := strings.Split(resourceName, "/")
				if len(parts) >= 3 {
					// parts[0] = uploads
					// parts[1] = uuid
					// parts[2:] = rest
					canonical := strings.Join(parts[2:], "/")
					m.stored[canonical] = buf.Bytes()
				}
			}
			m.stored[resourceName] = buf.Bytes()
			return stream.SendAndClose(&bytestream.WriteResponse{CommittedSize: int64(buf.Len())})
		}
		if err != nil {
			return err
		}
		if req.ResourceName != "" {
			resourceName = req.ResourceName
		}
		buf.Write(req.Data)
	}
}

func TestRemoteStore_Compression(t *testing.T) {
	// Setup Mock Server
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	mock := &mockByteStream{stored: make(map[string][]byte)}
	caps := &mockCapabilities{}
	s := grpc.NewServer()
	bytestream.RegisterByteStreamServer(s, mock)
	repb.RegisterCapabilitiesServer(s, caps)
	go s.Serve(lis)
	defer s.Stop()

	// Client
	ctx := context.Background()
	remote, err := NewRemoteStore(ctx, config.BackingCacheConfig{
		Target:      lis.Addr().String(),
		Compression: "zstd",
	})
	if err != nil {
		t.Fatalf("NewRemoteStore failed: %v", err)
	}
	defer remote.Close()

	// Test Data
	data := []byte("hello world repeated " + string(make([]byte, 100)))
	digest := Digest{Hash: "hash1", Size: int64(len(data))}

	// 1. Put (Should compress)
	err = remote.Put(ctx, digest, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// Verify server received compressed data
	found := false
	for name, content := range mock.stored {
		if strings.Contains(name, "compressed-blobs/zstd") {
			found = true
			if len(content) >= len(data) {
				// Very small data might not compress well, but with 100 zeros it should.
				t.Errorf("Stored content size %d >= original %d, expected compression", len(content), len(data))
			}
			// Verify it is valid zstd
			dec, err := zstd.NewReader(bytes.NewReader(content))
			if err != nil {
				t.Errorf("Stored content is not valid zstd: %v", err)
			}
			decoded, _ := io.ReadAll(dec)
			dec.Close()
			if !bytes.Equal(decoded, data) {
				t.Errorf("Decoded content mismatch")
			}
		}
	}
	if !found {
		t.Error("No compressed blob found in mock store")
	}

	// 2. Get (Should decompress)
	// mock.stored already has the compressed data at the right resource name
	rc, err := remote.Get(ctx, digest)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}

	if !bytes.Equal(got, data) {
		t.Errorf("Get returned wrong data")
	}
}
