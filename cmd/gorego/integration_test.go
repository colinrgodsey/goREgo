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
	"google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
)

func TestIntegration_CAS(t *testing.T) {
	// 1. Setup Server
	tempDir1 := t.TempDir()
	tempDir2 := t.TempDir()
	
	localStore1, err := storage.NewLocalStore(tempDir1, false)
	if err != nil {
		t.Fatalf("Failed to create local store 1: %v", err)
	}
	localStore2, err := storage.NewLocalStore(tempDir2, false)
	if err != nil {
		t.Fatalf("Failed to create local store 2: %v", err)
	}

	// Use two tiers
	proxyStore := proxy.NewProxyStore(localStore1, localStore2)
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
	bytestream.RegisterByteStreamServer(grpcServer, server.NewByteStreamServer(proxyStore))
	repb.RegisterActionCacheServer(grpcServer, server.NewActionCacheServer(proxyStore))

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
	err = localStore1.Put(ctx, blobDigest, bytes.NewReader(blobContent))
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

	// D. ByteStream Write
	blob2Content := []byte("ByteStream data")
	_, err = c.WriteBlob(ctx, blob2Content)
	if err != nil {
		t.Fatalf("ByteStream Write failed: %v", err)
	}

	// E. ByteStream Read
	blob2Digest := digest.NewFromBlob(blob2Content)
	readContent, _, err := c.ReadBlob(ctx, blob2Digest)
	if err != nil {
		t.Fatalf("ByteStream Read failed: %v", err)
	}
	if !bytes.Equal(blob2Content, readContent) {
		t.Errorf("Read content mismatch. Expected %q, got %q", string(blob2Content), string(readContent))
	}

	// F. BatchUpdateBlobs
	blobs := map[digest.Digest][]byte{
		digest.NewFromBlob([]byte("Batch Blob 1")): []byte("Batch Blob 1"),
		digest.NewFromBlob([]byte("Batch Blob 2")): []byte("Batch Blob 2"),
	}
	if err := c.BatchWriteBlobs(ctx, blobs); err != nil {
		t.Fatalf("BatchWriteBlobs failed: %v", err)
	}

	// Verify they exist
	var digests []digest.Digest
	for d := range blobs {
		digests = append(digests, d)
	}
	missing, err = c.MissingBlobs(ctx, digests)
	if err != nil {
		t.Fatalf("MissingBlobs failed: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("Expected 0 missing blobs after batch update, got %d", len(missing))
	}

	// G. Action Cache
	actionDigest := digest.NewFromBlob([]byte("Action 1"))
	actionResult := &repb.ActionResult{ExitCode: 123}
	if _, err := c.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{
		InstanceName: "test-instance",
		ActionDigest: actionDigest.ToProto(),
		ActionResult: actionResult,
	}); err != nil {
		t.Fatalf("UpdateActionResult failed: %v", err)
	}

	gotResult, err := c.GetActionResult(ctx, &repb.GetActionResultRequest{
		InstanceName: "test-instance",
		ActionDigest: actionDigest.ToProto(),
	})
	if err != nil {
		t.Fatalf("GetActionResult failed: %v", err)
	}
	if gotResult.ExitCode != 123 {
		t.Errorf("ActionResult mismatch. Want 123, got %d", gotResult.ExitCode)
	}
}
