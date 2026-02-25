package janitor

import (
	"context"
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

func TestJanitor_Notify(t *testing.T) {
	j := &Janitor{
		rootDir:     "/cache",
		maxSize:     1000,
		debounceDur: 10 * time.Millisecond,
		fs:          newMockFileSystem(),
		notifyCh:    make(chan struct{}, 1),
	}

	// Test that Notify is non-blocking
	j.Notify()
	j.Notify() // Second call should not block (buffered channel)

	// Verify notification is in the channel
	select {
	case <-j.notifyCh:
		// Expected
	default:
		t.Error("Expected notification in channel")
	}

	// Verify channel is empty (second notify was dropped due to buffer)
	select {
	case <-j.notifyCh:
		t.Error("Did not expect second notification")
	default:
		// Expected
	}
}

func TestJanitor_OnPut(t *testing.T) {
	j := &Janitor{
		rootDir:     "/cache",
		maxSize:     1000,
		debounceDur: 10 * time.Millisecond,
		fs:          newMockFileSystem(),
		notifyCh:    make(chan struct{}, 1),
	}

	callback := j.OnPut()
	if callback == nil {
		t.Fatal("OnPut returned nil callback")
	}

	// Call the callback
	callback()

	// Verify notification was triggered
	select {
	case <-j.notifyCh:
		// Expected
	case <-time.After(100 * time.Millisecond):
		t.Error("Callback did not trigger notification")
	}
}

func TestJanitor_Debounce(t *testing.T) {
	mockFS := newMockFileSystem()
	now := time.Now()
	mockFS.addFile("file1", 100, now.Add(-1*time.Hour))
	mockFS.addFile("file2", 200, now.Add(-30*time.Minute))

	j := &Janitor{
		rootDir:     "/cache",
		maxSize:     150, // Should remove file1 to get under limit
		minAge:      1 * time.Millisecond,
		debounceDur: 50 * time.Millisecond,
		fs:          mockFS,
		notifyCh:    make(chan struct{}, 1),
	}

	// Start the Run loop in a goroutine
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = j.Run(ctx)
	}()

	// Send multiple notifications rapidly
	j.Notify()
	time.Sleep(10 * time.Millisecond)
	j.Notify()
	time.Sleep(10 * time.Millisecond)
	j.Notify()

	// Wait for Run to complete
	<-done

	// Check that cleanup happened (at least one file should be removed)
	if len(mockFS.removedFiles) < 1 {
		t.Errorf("Expected at least 1 file removed, got %d", len(mockFS.removedFiles))
	}
}
