package proxy

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/colinrgodsey/goREgo/pkg/storage"
)

// Mock implementations for testing
type mockBlobStore struct {
	storage.BlobStore
	hasFunc func(ctx context.Context, d storage.Digest) (bool, error)
	getFunc func(ctx context.Context, d storage.Digest) (io.ReadCloser, error)
	putFunc func(ctx context.Context, d storage.Digest, data io.Reader) error
}

func (m *mockBlobStore) Has(ctx context.Context, d storage.Digest) (bool, error) {
	if m.hasFunc != nil {
		return m.hasFunc(ctx, d)
	}
	return false, nil
}

func (m *mockBlobStore) Get(ctx context.Context, d storage.Digest) (io.ReadCloser, error) {
	if m.getFunc != nil {
		return m.getFunc(ctx, d)
	}
	return nil, nil
}

func (m *mockBlobStore) Put(ctx context.Context, d storage.Digest, data io.Reader) error {
	if m.putFunc != nil {
		return m.putFunc(ctx, d, data)
	}
	// Consume the reader to avoid "io: read/write on closed pipe" errors
	_, err := io.Copy(io.Discard, data)
	return err
}

// MockActionCache implements storage.ActionCache
type MockActionCache struct {
	storage.ActionCache
	getActionResultFunc    func(ctx context.Context, d storage.Digest) (*repb.ActionResult, error)
	updateActionResultFunc func(ctx context.Context, d storage.Digest, result *repb.ActionResult) error
}

func (m *MockActionCache) Has(ctx context.Context, d storage.Digest) (bool, error) {
	return false, nil
}

func (m *MockActionCache) Get(ctx context.Context, d storage.Digest) (io.ReadCloser, error) {
	return nil, nil
}

func (m *MockActionCache) Put(ctx context.Context, d storage.Digest, data io.Reader) error {
	return nil
}

func (m *MockActionCache) GetActionResult(ctx context.Context, d storage.Digest) (*repb.ActionResult, error) {
	if m.getActionResultFunc != nil {
		return m.getActionResultFunc(ctx, d)
	}
	return nil, nil
}

func (m *MockActionCache) UpdateActionResult(ctx context.Context, d storage.Digest, result *repb.ActionResult) error {
	if m.updateActionResultFunc != nil {
		return m.updateActionResultFunc(ctx, d, result)
	}
	return nil
}

// TestNewProxyStore tests the creation of a new ProxyStore
func TestNewProxyStore(t *testing.T) {
	tempDir := t.TempDir()
	localStore, err := storage.NewLocalStore(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to create local store: %v", err)
	}

	remoteStore := &mockBlobStore{}
	proxy := NewProxyStore(localStore, remoteStore, 3)

	if proxy == nil {
		t.Error("Expected ProxyStore to be created")
	}

	if proxy.putRetryCount != 3 {
		t.Errorf("Expected putRetryCount to be 3, got %d", proxy.putRetryCount)
	}
}

// TestProxyStore_BlobPath tests the BlobPath method
func TestProxyStore_BlobPath(t *testing.T) {
	tempDir := t.TempDir()
	localStore, err := storage.NewLocalStore(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to create local store: %v", err)
	}

	remoteStore := &mockBlobStore{}
	proxy := NewProxyStore(localStore, remoteStore, 3)

	testDigest := storage.Digest{
		Hash: "testhash",
		Size: 9, // "test data"
	}

	path, err := proxy.BlobPath(testDigest)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	if path == "" {
		t.Error("Expected non-empty path")
	}
}

// TestProxyStore_Has tests the Has method with local hit
func TestProxyStore_Has_LocalHit(t *testing.T) {
	tempDir := t.TempDir()
	localStore, err := storage.NewLocalStore(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to create local store: %v", err)
	}

	remoteStore := &mockBlobStore{}
	proxy := NewProxyStore(localStore, remoteStore, 3)

	testDigest := storage.Digest{
		Hash: "testhash",
		Size: 9, // "test data"
	}

	// Put data in local store first
	testData := []byte("test data")
	if err := localStore.Put(context.Background(), testDigest, bytes.NewReader(testData)); err != nil {
		t.Fatalf("Failed to put test data: %v", err)
	}

	has, err := proxy.Has(context.Background(), testDigest)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	if !has {
		t.Error("Expected true for Has when local has the digest")
	}
}

// TestProxyStore_Has tests the Has method with remote hit
func TestProxyStore_Has_RemoteHit(t *testing.T) {
	tempDir := t.TempDir()
	localStore, err := storage.NewLocalStore(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to create local store: %v", err)
	}

	remoteStore := &mockBlobStore{
		hasFunc: func(ctx context.Context, d storage.Digest) (bool, error) {
			return true, nil
		},
	}

	proxy := NewProxyStore(localStore, remoteStore, 3)

	testDigest := storage.Digest{
		Hash: "testhash",
		Size: 9, // "test data"
	}

	has, err := proxy.Has(context.Background(), testDigest)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	if !has {
		t.Error("Expected true for Has when remote has the digest")
	}
}

// TestProxyStore_Has tests the Has method with no hit
func TestProxyStore_Has_NoHit(t *testing.T) {
	tempDir := t.TempDir()
	localStore, err := storage.NewLocalStore(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to create local store: %v", err)
	}

	remoteStore := &mockBlobStore{
		hasFunc: func(ctx context.Context, d storage.Digest) (bool, error) {
			return false, nil
		},
	}

	proxy := NewProxyStore(localStore, remoteStore, 3)

	testDigest := storage.Digest{
		Hash: "testhash",
		Size: 9, // "test data"
	}

	has, err := proxy.Has(context.Background(), testDigest)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	if has {
		t.Error("Expected false for Has when neither local nor remote has the digest")
	}
}

// TestProxyStore_Get tests the Get method with local hit
func TestProxyStore_Get_LocalHit(t *testing.T) {
	tempDir := t.TempDir()
	localStore, err := storage.NewLocalStore(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to create local store: %v", err)
	}

	remoteStore := &mockBlobStore{}
	proxy := NewProxyStore(localStore, remoteStore, 3)

	testDigest := storage.Digest{
		Hash: "testhash",
		Size: 9, // "test data"
	}

	testData := []byte("test data")
	if err := localStore.Put(context.Background(), testDigest, bytes.NewReader(testData)); err != nil {
		t.Fatalf("Failed to put test data: %v", err)
	}

	rc, err := proxy.Get(context.Background(), testDigest)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	if rc == nil {
		t.Error("Expected non-nil ReadCloser")
	}

	defer rc.Close()

	buf := new(bytes.Buffer)
	_, err = buf.ReadFrom(rc)
	if err != nil {
		t.Errorf("Failed to read from reader: %v", err)
	}

	if !bytes.Equal(buf.Bytes(), testData) {
		t.Error("Expected data to match")
	}
}

// TestProxyStore_Get tests the Get method with remote hit and local caching
func TestProxyStore_Get_RemoteHit(t *testing.T) {
	tempDir := t.TempDir()
	localStore, err := storage.NewLocalStore(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to create local store: %v", err)
	}

	testData := []byte("remote test data")
	remoteStore := &mockBlobStore{
		getFunc: func(ctx context.Context, d storage.Digest) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(testData)), nil
		},
	}

	proxy := NewProxyStore(localStore, remoteStore, 3)

	testDigest := storage.Digest{
		Hash: "testhash",
		Size: 16, // "remote test data"
	}

	rc, err := proxy.Get(context.Background(), testDigest)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	if rc == nil {
		t.Error("Expected non-nil ReadCloser")
	}

	defer rc.Close()

	buf := new(bytes.Buffer)
	_, err = buf.ReadFrom(rc)
	if err != nil {
		t.Errorf("Failed to read from reader: %v", err)
	}

	if !bytes.Equal(buf.Bytes(), testData) {
		t.Error("Expected data to match")
	}
}

// TestProxyStore_Put tests the Put method with early local hit
func TestProxyStore_Put_LocalHit(t *testing.T) {
	tempDir := t.TempDir()
	localStore, err := storage.NewLocalStore(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to create local store: %v", err)
	}

	remoteStore := &mockBlobStore{}
	proxy := NewProxyStore(localStore, remoteStore, 3)

	testDigest := storage.Digest{
		Hash: "testhash",
		Size: 9, // "test data"
	}

	// Put data to local first
	testData := []byte("test data")
	if err := localStore.Put(context.Background(), testDigest, bytes.NewReader(testData)); err != nil {
		t.Fatalf("Failed to put test data: %v", err)
	}

	// This should be a no-op since it already exists locally
	newTestData := []byte("new test data")
	err = proxy.Put(context.Background(), testDigest, bytes.NewReader(newTestData))
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	// Verify local data is unchanged
	rc, err := localStore.Get(context.Background(), testDigest)
	if err != nil {
		t.Errorf("Failed to get from local store: %v", err)
	}
	defer rc.Close()

	buf := new(bytes.Buffer)
	_, err = buf.ReadFrom(rc)
	if err != nil {
		t.Errorf("Failed to read from reader: %v", err)
	}

	if !bytes.Equal(buf.Bytes(), testData) {
		t.Error("Expected local data to remain unchanged")
	}
}

// TestProxyStore_Put tests the Put method with remote upload failure
func TestProxyStore_Put_RemoteFailure(t *testing.T) {
	tempDir := t.TempDir()
	localStore, err := storage.NewLocalStore(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to create local store: %v", err)
	}

	remoteStore := &mockBlobStore{
		putFunc: func(ctx context.Context, d storage.Digest, data io.Reader) error {
			return os.ErrPermission
		},
	}

	proxy := NewProxyStore(localStore, remoteStore, 3)

	testDigest := storage.Digest{
		Hash: "testhash",
		Size: 9, // "test data"
	}

	testData := []byte("test data")
	err = proxy.Put(context.Background(), testDigest, bytes.NewReader(testData))
	if err == nil {
		t.Error("Expected error from remote put failure")
	}
}

// TestProxyStore_Put tests the Put method with successful upload
func TestProxyStore_Put_Success(t *testing.T) {
	tempDir := t.TempDir()
	localStore, err := storage.NewLocalStore(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to create local store: %v", err)
	}

	remoteStore := &mockBlobStore{}
	proxy := NewProxyStore(localStore, remoteStore, 3)

	testDigest := storage.Digest{
		Hash: "testhash",
		Size: 9, // "test data"
	}

	testData := []byte("test data")
	err = proxy.Put(context.Background(), testDigest, bytes.NewReader(testData))
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	// Verify data exists in both stores
	hasLocal, err := localStore.Has(context.Background(), testDigest)
	if err != nil {
		t.Errorf("Failed to check local store: %v", err)
	}

	if !hasLocal {
		t.Error("Expected data to exist in local store")
	}
}

// TestProxyStore_PutFile tests the PutFile method
func TestProxyStore_PutFile(t *testing.T) {
	tempDir := t.TempDir()
	localStore, err := storage.NewLocalStore(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to create local store: %v", err)
	}

	remoteStore := &mockBlobStore{}
	proxy := NewProxyStore(localStore, remoteStore, 3)

	testData := []byte("test file data")
	tmpFile, err := os.CreateTemp("", "testfile")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.Write(testData)
	if err != nil {
		t.Fatalf("Failed to write test data to temp file: %v", err)
	}
	tmpFile.Close()

	testDigest := storage.Digest{
		Hash: "testhash",
		Size: 14, // Size of "test file data"
	}

	err = proxy.PutFile(context.Background(), testDigest, tmpFile.Name())
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	// Verify data exists in both stores
	hasLocal, err := localStore.Has(context.Background(), testDigest)
	if err != nil {
		t.Errorf("Failed to check local store: %v", err)
	}

	if !hasLocal {
		t.Error("Expected data to exist in local store")
	}
}

// TestProxyStore_GetActionResult tests the GetActionResult method with local hit
func TestProxyStore_GetActionResult_LocalHit(t *testing.T) {
	tempDir := t.TempDir()
	localStore, err := storage.NewLocalStore(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to create local store: %v", err)
	}

	remoteStore := &mockBlobStore{}
	proxy := NewProxyStore(localStore, remoteStore, 3)

	testDigest := storage.Digest{
		Hash: "testhash",
		Size: 9, // "test data"
	}

	// Create a mock action result
	actionResult := &repb.ActionResult{
		ExitCode: 0,
	}

	// Put action result in local store first
	if err := localStore.UpdateActionResult(context.Background(), testDigest, actionResult); err != nil {
		t.Fatalf("Failed to put test action result: %v", err)
	}

	result, err := proxy.GetActionResult(context.Background(), testDigest)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	if result == nil {
		t.Error("Expected non-nil ActionResult")
	}

	if result.ExitCode != 0 {
		t.Error("Expected ActionResult exit code to match")
	}
}

// TestProxyStore_GetActionResult tests the GetActionResult method with remote hit
func TestProxyStore_GetActionResult_RemoteHit(t *testing.T) {
	tempDir := t.TempDir()
	localStore, err := storage.NewLocalStore(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to create local store: %v", err)
	}

	testActionResult := &repb.ActionResult{
		ExitCode:     0,
		StdoutDigest: &repb.Digest{Hash: "stdouthash", SizeBytes: 10},
		StderrDigest: &repb.Digest{Hash: "stderrhash", SizeBytes: 10},
	}

	remoteStore := &MockActionCache{
		getActionResultFunc: func(ctx context.Context, d storage.Digest) (*repb.ActionResult, error) {
			return testActionResult, nil
		},
	}

	proxy := NewProxyStore(localStore, remoteStore, 3)

	testDigest := storage.Digest{
		Hash: "testhash",
		Size: 9, // "test data"
	}

	result, err := proxy.GetActionResult(context.Background(), testDigest)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	if result == nil {
		t.Error("Expected non-nil ActionResult")
	}

	if result.ExitCode != 0 {
		t.Error("Expected ActionResult exit code to match")
	}
}

// TestProxyStore_UpdateActionResult tests the UpdateActionResult method
func TestProxyStore_UpdateActionResult(t *testing.T) {
	tempDir := t.TempDir()
	localStore, err := storage.NewLocalStore(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to create local store: %v", err)
	}

	remoteStore := &MockActionCache{
		updateActionResultFunc: func(ctx context.Context, d storage.Digest, result *repb.ActionResult) error {
			return nil
		},
	}

	proxy := NewProxyStore(localStore, remoteStore, 3)

	testDigest := storage.Digest{
		Hash: "testhash",
		Size: 9, // "test data"
	}

	actionResult := &repb.ActionResult{
		ExitCode: 0,
	}

	err = proxy.UpdateActionResult(context.Background(), testDigest, actionResult)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
}
