package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLocalStore_PutFile_Hardlink(t *testing.T) {
	tempDir := t.TempDir()
	store, err := NewLocalStore(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to create local store: %v", err)
	}

	// Create a source file
	srcPath := filepath.Join(tempDir, "source.txt")
	content := []byte("hello world")
	if err := os.WriteFile(srcPath, content, 0644); err != nil {
		t.Fatalf("Failed to write source file: %v", err)
	}

	// Calculate digest
	hash := sha256.Sum256(content)
	digest := Digest{
		Hash: hex.EncodeToString(hash[:]),
		Size: int64(len(content)),
	}

	// PutFile
	ctx := context.Background()
	if err := store.PutFile(ctx, digest, srcPath); err != nil {
		t.Fatalf("PutFile failed: %v", err)
	}

	// Verify it exists in CAS
	casPath, err := store.BlobPath(digest)
	if err != nil {
		t.Fatalf("BlobPath failed: %v", err)
	}

	if _, err := os.Stat(casPath); err != nil {
		t.Fatalf("CAS file not found: %v", err)
	}

	// Verify content
	data, err := os.ReadFile(casPath)
	if err != nil {
		t.Fatalf("Failed to read CAS file: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("Content mismatch: expected %q, got %q", content, data)
	}

	// Verify hardlink (same inode)
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		t.Fatalf("Failed to stat source: %v", err)
	}
	casInfo, err := os.Stat(casPath)
	if err != nil {
		t.Fatalf("Failed to stat CAS file: %v", err)
	}

	srcStat, ok1 := srcInfo.Sys().(*syscall.Stat_t)
	casStat, ok2 := casInfo.Sys().(*syscall.Stat_t)

	if ok1 && ok2 {
		if srcStat.Ino != casStat.Ino {
			t.Errorf("Expected same inode (hardlink), got %d and %d", srcStat.Ino, casStat.Ino)
		}
	} else {
		t.Skip("Skipping inode check (not supported on this platform)")
	}
}
