package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/colinrgodsey/goREgo/pkg/proxy"
	"github.com/colinrgodsey/goREgo/pkg/server"
	"github.com/colinrgodsey/goREgo/pkg/storage"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestIntegration_Compression(t *testing.T) {
	// Setup Server
	tempDir := t.TempDir()
	store, _ := storage.NewLocalStore(tempDir, false)
	// Use NullStore as remote for simplicity
	proxyStore := proxy.NewProxyStore(store, storage.NewNullStore())

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Failed to listen: %v", err)
	}
	addr := lis.Addr().String()

	grpcServer := grpc.NewServer()
	bytestream.RegisterByteStreamServer(grpcServer, server.NewByteStreamServer(proxyStore))
	repb.RegisterCapabilitiesServer(grpcServer, server.NewCapabilitiesServer(false))

	go func() {
		_ = grpcServer.Serve(lis)
	}()
	defer grpcServer.Stop()

	// Connect
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	bsClient := bytestream.NewByteStreamClient(conn)
	ctx := context.Background()

	// Test Data
	rawData := []byte("Compression test data " + string(make([]byte, 1000))) // Compressible

	// Calculate real digest
	hasher := sha256.New()
	hasher.Write(rawData)
	hash := hex.EncodeToString(hasher.Sum(nil))

	size := int64(len(rawData))
	digest := storage.Digest{
		Hash: hash,
		Size: size,
	}

	// 1. Write Compressed
	// Compress data
	var compressedBuf bytes.Buffer
	enc, _ := zstd.NewWriter(&compressedBuf)
	enc.Write(rawData)
	enc.Close()

	// Resource name: uploads/uuid/compressed-blobs/zstd/hash/size
	resourceName := fmt.Sprintf("uploads/uuid/compressed-blobs/zstd/%s/%d", digest.Hash, digest.Size)

	stream, err := bsClient.Write(ctx)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	err = stream.Send(&bytestream.WriteRequest{
		ResourceName: resourceName,
		Data:         compressedBuf.Bytes(),
		FinishWrite:  true,
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	_, err = stream.CloseAndRecv()
	if err != nil {
		t.Fatalf("CloseAndRecv failed: %v", err)
	}

	// Verify it is stored UNCOMPRESSED in LocalStore
	rc, err := store.Get(ctx, digest)
	if err != nil {
		t.Fatalf("Get from store failed: %v", err)
	}
	storedData, _ := io.ReadAll(rc)
	rc.Close()

	if !bytes.Equal(storedData, rawData) {
		t.Errorf("Stored data mismatch. Got len: %d, want len: %d", len(storedData), len(rawData))
	}

	// 2. Read Compressed
	readResourceName := fmt.Sprintf("compressed-blobs/zstd/%s/%d", digest.Hash, digest.Size)
	readStream, err := bsClient.Read(ctx, &bytestream.ReadRequest{
		ResourceName: readResourceName,
	})
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	var readCompressed []byte
	for {
		resp, err := readStream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv failed: %v", err)
		}
		readCompressed = append(readCompressed, resp.Data...)
	}

	// Decompress client side
	dec, _ := zstd.NewReader(bytes.NewReader(readCompressed))
	decompressedData, _ := io.ReadAll(dec)
	dec.Close()

	if !bytes.Equal(decompressedData, rawData) {
		t.Errorf("Read data mismatch")
	}
}
