package storage

import (
	"bytes"
	"context"
	"io"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
)

func TestLocalStore(t *testing.T) {
	tempDir := t.TempDir()
	store, err := NewLocalStore(tempDir, false)
	if err != nil {
		t.Fatalf("NewLocalStore failed: %v", err)
	}

	ctx := context.Background()
	content := []byte("hello world")
	// Using a dummy hash for testing
	d := Digest{Hash: "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9", Size: int64(len(content))}

	// Test Put
	err = store.Put(ctx, d, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// Test Has
	has, err := store.Has(ctx, d)
	if err != nil || !has {
		t.Fatalf("Has failed: %v, %v", has, err)
	}

	// Test Get
	rc, err := store.Get(ctx, d)
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
	err = store.UpdateActionResult(ctx, acDigest, ar)
	if err != nil {
		t.Fatalf("UpdateActionResult failed: %v", err)
	}

	gotAr, err := store.GetActionResult(ctx, acDigest)
	if err != nil || gotAr.ExitCode != 0 {
		t.Fatalf("GetActionResult failed or mismatch: %v, %v", gotAr, err)
	}
}