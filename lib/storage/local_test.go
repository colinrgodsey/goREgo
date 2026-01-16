package storage

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
)

func TestLocalStore(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "gorego-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	s, err := NewLocalStore(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	content := []byte("hello world")
	digest := Digest{Hash: "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9", Size: int64(len(content))}

	// Test Put
	err = s.Put(ctx, digest, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// Test Has
	has, err := s.Has(ctx, digest)
	if err != nil || !has {
		t.Fatalf("Has failed: %v, %v", has, err)
	}

	// Test Get
	rc, err := s.Get(ctx, digest)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("Content mismatch: got %q, want %q, err: %v", got, content, err)
	}

	// Test ActionCache
	ar := &repb.ActionResult{
		ExitCode: 0,
	}
	acDigest := Digest{Hash: "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890", Size: 100}
	err = s.UpdateActionResult(ctx, acDigest, ar)
	if err != nil {
		t.Fatalf("UpdateActionResult failed: %v", err)
	}

	gotAr, err := s.GetActionResult(ctx, acDigest)
	if err != nil || gotAr.ExitCode != 0 {
		t.Fatalf("GetActionResult failed or mismatch: %v, %v", gotAr, err)
	}
}
