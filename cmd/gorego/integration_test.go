package main

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/bazelbuild/remote-apis-sdks/go/pkg/client"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/digest"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/colinrgodsey/goREgo/pkg/proxy"
	"github.com/colinrgodsey/goREgo/pkg/server"
	"github.com/colinrgodsey/goREgo/pkg/storage"
	"google.golang.org/grpc"
)

func TestIntegration_CAS(t *testing.T) {
	// 1. Setup Server
	tempDir := t.TempDir()
	localStore, err := storage.NewLocalStore(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to create local store: %v", err)
	}

	// Use local store as both tiers for now (Passthrough mode)
	proxyStore := proxy.NewProxyStore(localStore, localStore)
	casServer := server.NewContentAddressableStorageServer(proxyStore)

	// Start listener on random port
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Failed to listen: %v", err)
	}
	addr := lis.Addr().String()

	grpcServer := grpc.NewServer()
	repb.RegisterContentAddressableStorageServer(grpcServer, casServer)
	repb.RegisterCapabilitiesServer(grpcServer, server.NewCapabilitiesServer())

	// Start server in background
	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			// Server stopped
		}
	}()
	defer grpcServer.Stop()

	// 2. Setup Client (SDK)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := client.NewClient(ctx, "test-instance", client.DialParams{
		Service:    addr,
		NoSecurity: true,
	})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	defer c.Close()

	// 3. Perform Test Operations
	blobContent := []byte("Hello, Integrated World!")
	blobDigest := digest.NewFromBlob(blobContent)

	// A. FindMissingBlobs (Should be missing)
	missing, err := c.MissingBlobs(ctx, []digest.Digest{blobDigest})
	if err != nil {
		t.Fatalf("MissingBlobs failed: %v", err)
	}
	if len(missing) != 1 {
		t.Errorf("Expected 1 missing blob, got %d", len(missing))
	}

	// B. Put blob directly in store (simulating an upload)
	err = localStore.Put(ctx, blobDigest, bytes.NewReader(blobContent))
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// C. FindMissingBlobs (Should NOT be missing)
	missing, err = c.MissingBlobs(ctx, []digest.Digest{blobDigest})
	if err != nil {
		t.Fatalf("MissingBlobs failed: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("Expected 0 missing blobs, got %d", len(missing))
	}
}
