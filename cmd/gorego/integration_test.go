package main

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/digest"
	"github.com/colinrgodsey/goREgo/lib/proxy"
	"github.com/colinrgodsey/goREgo/lib/server"
	"github.com/colinrgodsey/goREgo/lib/storage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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

	// Start server in background
	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			// Server stopped
		}
	}()
	defer grpcServer.Stop()

	// 2. Setup Client (raw gRPC)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("Failed to dial: %v", err)
	}
	defer conn.Close()

	casClient := repb.NewContentAddressableStorageClient(conn)

	// 3. Perform Test Operations
	blobContent := []byte("Hello, Integrated World!")
	blobDigest := digest.NewFromBlob(blobContent)
	blobDigestProto := blobDigest.ToProto()

	// A. FindMissingBlobs (Should be missing)
	missingResp, err := casClient.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{
		BlobDigests: []*repb.Digest{blobDigestProto},
	})
	if err != nil {
		t.Fatalf("FindMissingBlobs failed: %v", err)
	}
	if len(missingResp.MissingBlobDigests) != 1 {
		t.Errorf("Expected 1 missing blob, got %d", len(missingResp.MissingBlobDigests))
	}

	// B. Put blob directly in store (simulating an upload)
	err = localStore.Put(ctx, blobDigest, bytes.NewReader(blobContent))
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// C. FindMissingBlobs (Should NOT be missing)
	missingResp, err = casClient.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{
		BlobDigests: []*repb.Digest{blobDigestProto},
	})
	if err != nil {
		t.Fatalf("FindMissingBlobs failed: %v", err)
	}
	if len(missingResp.MissingBlobDigests) != 0 {
		t.Errorf("Expected 0 missing blobs, got %d", len(missingResp.MissingBlobDigests))
	}
}
