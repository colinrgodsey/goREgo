package janitor

import (
	"fmt"
	"sort"
	"testing"
	"time"
)

// Mock FileEntry
type mockFileEntry struct {
	name       string
	size       int64
	isDir      bool
	accessTime time.Time
}

func (e *mockFileEntry) Name() string          { return e.name }
func (e *mockFileEntry) Size() int64           { return e.size }
func (e *mockFileEntry) IsDir() bool           { return e.isDir }
func (e *mockFileEntry) AccessTime() time.Time { return e.accessTime }

// Mock FileSystem
type mockFileSystem struct {
	files map[string]*mockFileEntry
	// Tracking calls
	removedFiles []string
}

func newMockFileSystem() *mockFileSystem {
	return &mockFileSystem{
		files: make(map[string]*mockFileEntry),
	}
}

func (fs *mockFileSystem) Walk(root string, fn func(path string, info fileEntry, err error) error) error {
	// Simulate deterministic walk order (e.g. by path) to ensure test stability,
	// though map iteration is random.
	var paths []string
	for p := range fs.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, path := range paths {
		entry := fs.files[path]
		if err := fn(path, entry, nil); err != nil {
			return err
		}
	}
	return nil
}

func (fs *mockFileSystem) Remove(path string) error {
	if _, ok := fs.files[path]; !ok {
		return fmt.Errorf("file not found: %s", path)
	}
	delete(fs.files, path)
	fs.removedFiles = append(fs.removedFiles, path)
	return nil
}

// Helper to add files
func (fs *mockFileSystem) addFile(path string, size int64, accessTime time.Time) {
	fs.files[path] = &mockFileEntry{
		name:       path,
		size:       size,
		isDir:      false,
		accessTime: accessTime,
	}
}

func TestJanitor_Cleanup(t *testing.T) {
	// Setup Mock FS
	mockFS := newMockFileSystem()
	now := time.Now()

	// Add files: Total size = 100 + 200 + 300 = 600 bytes
	mockFS.addFile("file1_oldest", 100, now.Add(-3*time.Hour))
	mockFS.addFile("file2_middle", 200, now.Add(-2*time.Hour))
	mockFS.addFile("file3_newest", 300, now.Add(-1*time.Hour))

	// Config: Max size = 400 bytes (should trigger deletion of oldest)
	// Note: Config expects GB, so we need to hack the Janitor struct directly or use a tiny GB fraction if it was float,
	// but it is int.
	// So we will instantiate Janitor manually with injected FS and custom maxSize.
	
	j := &Janitor{
		rootDir: "/cache",
		maxSize: 400, // Bytes
		fs:      mockFS,
	}

	// Run Cleanup
	if err := j.Cleanup(); err != nil {
		t.Fatalf("Cleanup failed: %v", err)
	}

	// Verify Logic
	// Total was 600. Max is 400.
	// Should remove oldest (file1: 100b). New Total: 500. Still > 400?
	// Wait, loop continues until <= maxSize.
	// 600 - 100 (file1) = 500. Still > 400.
	// Should remove next oldest (file2: 200b). New Total: 300. <= 400. Done.
	// So file1 and file2 should be removed.

	if len(mockFS.files) != 1 {
		t.Errorf("Expected 1 file remaining, got %d", len(mockFS.files))
	}
	if _, ok := mockFS.files["file3_newest"]; !ok {
		t.Error("file3_newest should remain")
	}

	expectedRemoved := 2
	if len(mockFS.removedFiles) != expectedRemoved {
		t.Errorf("Expected %d removed files, got %d: %v", expectedRemoved, len(mockFS.removedFiles), mockFS.removedFiles)
	}
}

func TestJanitor_NoCleanupNeeded(t *testing.T) {
	mockFS := newMockFileSystem()
	mockFS.addFile("file1", 100, time.Now())

	j := &Janitor{
		rootDir: "/cache",
		maxSize: 1000,
		fs:      mockFS,
	}

	if err := j.Cleanup(); err != nil {
		t.Fatalf("Cleanup failed: %v", err)
	}

	if len(mockFS.removedFiles) != 0 {
		t.Errorf("Expected no files removed, got %v", mockFS.removedFiles)
	}
}
