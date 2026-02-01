package storage

import (
	"context"
	"net"
	"runtime"
	"testing"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/colinrgodsey/goREgo/pkg/config"
	"google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
)

// Define mock types again as they are private in this package (if not exported)
// To avoid conflicts with remote_test.go if they are in the same package (storage),
// we use distinct names.

type mockCapabilitiesLeak struct {
	repb.UnimplementedCapabilitiesServer
}

func (m *mockCapabilitiesLeak) GetCapabilities(ctx context.Context, req *repb.GetCapabilitiesRequest) (*repb.ServerCapabilities, error) {
	return &repb.ServerCapabilities{
		CacheCapabilities: &repb.CacheCapabilities{
			DigestFunctions: []repb.DigestFunction_Value{repb.DigestFunction_SHA256},
		},
	}, nil
}

type mockByteStreamLeak struct {
	bytestream.UnimplementedByteStreamServer
	// control when Recv returns
	recvChan chan struct{}
}

func (m *mockByteStreamLeak) Read(req *bytestream.ReadRequest, stream bytestream.ByteStream_ReadServer) error {
	return nil
}

func (m *mockByteStreamLeak) Write(stream bytestream.ByteStream_WriteServer) error {
	// Simulate server that stops reading (blocking client Send)
	if m.recvChan != nil {
		<-m.recvChan
	}
	for {
		_, err := stream.Recv()
		if err != nil {
			return err
		}
	}
}

func TestRemoteStore_Put_Leak(t *testing.T) {
	// Setup Mock Server
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	// Make the server block on Write
	mock := &mockByteStreamLeak{recvChan: make(chan struct{})}
	caps := &mockCapabilitiesLeak{}
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

	// Warmup to ensure lazy goroutines are started
	remote.Has(ctx, Digest{Hash: "warmup", Size: 1})

	// Measure baseline
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	startRoutines := runtime.NumGoroutine()

	// Run Put in a separate goroutine
	putCtx, cancel := context.WithCancel(context.Background())

	// Create an infinite reader to keep the compressor busy
	// zeroReader always returns data, never EOF.
	input := &zeroReader{}
	digest := Digest{Hash: "hash1", Size: 1024 * 1024 * 100} // Large size

	done := make(chan error)
	go func() {
		err := remote.Put(putCtx, digest, input)
		done <- err
	}()

	// Wait for Put to start and fill buffers.
	// Since server blocks on Recv, client Send will eventually block.
	// The compressor goroutine will block on writing to 'pw' (pipe to Put).
	time.Sleep(200 * time.Millisecond)

	// Cancel context. Put should return error.
	cancel()

	// Put should return
	select {
	case <-done:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("Put did not return after context cancel")
	}

	// At this point, Put has returned.
	// But 'input' (zeroReader) is infinite and NOT closed.
	// The compressor goroutine in Put is:
	//   io.Copy(enc, input) -> writes to pw
	// Since Put returned, it stopped reading 'pr'.
	// 'pw' blocks on Write.
	// 'input' Read is valid.
	// So the goroutine is blocked on 'pw.Write'.
	// This is the leak.

	// Unblock server handler so it can exit
	close(mock.recvChan)

	// Allow time for goroutines to exit
	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	endRoutines := runtime.NumGoroutine()

	if endRoutines > startRoutines {
		t.Fatalf("Goroutine leak detected: start=%d, end=%d", startRoutines, endRoutines)
	}
}

type zeroReader struct{}

func (z *zeroReader) Read(p []byte) (n int, err error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
