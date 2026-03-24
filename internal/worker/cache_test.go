package worker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vyprai/loka/internal/objstore"
)

// mockObjStore implements objstore.ObjectStore for testing.
type mockObjStore struct {
	mu       sync.Mutex
	getCalls int
	data     map[string][]byte
}

func newMockObjStore() *mockObjStore {
	return &mockObjStore{data: make(map[string][]byte)}
}

func (m *mockObjStore) Get(_ context.Context, _, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalls++
	data, ok := m.data[key]
	if !ok {
		return nil, fmt.Errorf("not found: %s", key)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *mockObjStore) Put(_ context.Context, _, _ string, _ io.Reader, _ int64) error {
	return nil
}

func (m *mockObjStore) Delete(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockObjStore) Exists(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

func (m *mockObjStore) GetPresignedURL(_ context.Context, _, _ string, _ time.Duration) (string, error) {
	return "", nil
}

func (m *mockObjStore) List(_ context.Context, _, _ string) ([]objstore.ObjectInfo, error) {
	return nil, nil
}

func (m *mockObjStore) getCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getCalls
}

func newTestCache(t *testing.T, store *mockObjStore, maxSize int64, ttl time.Duration) *Cache {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewCacheWithOptions(dir, store, "test-bucket", maxSize, ttl, logger)
}

func TestCacheGetMiss(t *testing.T) {
	store := newMockObjStore()
	store.data["file.tar"] = []byte("file content here")

	cache := newTestCache(t, store, 0, 0)

	path, err := cache.Get(context.Background(), "file.tar")
	require.NoError(t, err)
	assert.FileExists(t, path)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "file content here", string(content))
	assert.Equal(t, 1, store.getCallCount())
}

func TestCacheGetHit(t *testing.T) {
	store := newMockObjStore()
	store.data["file.tar"] = []byte("cached data")

	cache := newTestCache(t, store, 0, 0)

	// First get — download.
	path1, err := cache.Get(context.Background(), "file.tar")
	require.NoError(t, err)
	assert.Equal(t, 1, store.getCallCount())

	// Second get — cache hit, no download.
	path2, err := cache.Get(context.Background(), "file.tar")
	require.NoError(t, err)
	assert.Equal(t, path1, path2)
	assert.Equal(t, 1, store.getCallCount())
}

func TestCacheTTLExpiry(t *testing.T) {
	store := newMockObjStore()
	store.data["file.tar"] = []byte("data")

	ttl := 100 * time.Millisecond
	cache := newTestCache(t, store, 0, ttl)

	// First get.
	_, err := cache.Get(context.Background(), "file.tar")
	require.NoError(t, err)
	assert.Equal(t, 1, store.getCallCount())

	// Wait for TTL to expire.
	time.Sleep(200 * time.Millisecond)

	// Second get — should re-download.
	_, err = cache.Get(context.Background(), "file.tar")
	require.NoError(t, err)
	assert.Equal(t, 2, store.getCallCount())
}

func TestCacheEviction(t *testing.T) {
	store := newMockObjStore()
	// Create data larger than maxSize to trigger eviction.
	store.data["old.tar"] = bytes.Repeat([]byte("A"), 600)
	store.data["new.tar"] = bytes.Repeat([]byte("B"), 600)

	maxSize := int64(1024) // 1KB
	cache := newTestCache(t, store, maxSize, 0)

	// Cache first file.
	oldPath, err := cache.Get(context.Background(), "old.tar")
	require.NoError(t, err)
	assert.FileExists(t, oldPath)

	// Ensure different modtime so eviction picks the right file.
	time.Sleep(10 * time.Millisecond)

	// Cache second file — total exceeds maxSize, so old.tar should be evicted.
	newPath, err := cache.Get(context.Background(), "new.tar")
	require.NoError(t, err)
	assert.FileExists(t, newPath)

	// Verify old.tar was evicted.
	_, err = os.Stat(oldPath)
	assert.True(t, os.IsNotExist(err), "old.tar should have been evicted")
}

func TestCacheInvalidate(t *testing.T) {
	store := newMockObjStore()
	store.data["file.tar"] = []byte("data")

	cache := newTestCache(t, store, 0, 0)

	// Download and cache.
	_, err := cache.Get(context.Background(), "file.tar")
	require.NoError(t, err)
	assert.Equal(t, 1, store.getCallCount())

	// Invalidate.
	require.NoError(t, cache.Invalidate("file.tar"))

	// Next get should re-download.
	_, err = cache.Get(context.Background(), "file.tar")
	require.NoError(t, err)
	assert.Equal(t, 2, store.getCallCount())
}

func TestCacheExists(t *testing.T) {
	store := newMockObjStore()
	store.data["file.tar"] = []byte("data")

	cache := newTestCache(t, store, 0, 0)

	// Not cached yet.
	assert.False(t, cache.Exists("file.tar"))

	// Cache it.
	_, err := cache.Get(context.Background(), "file.tar")
	require.NoError(t, err)

	// Now it exists.
	assert.True(t, cache.Exists("file.tar"))
}

func TestCacheExistsTTL(t *testing.T) {
	store := newMockObjStore()
	store.data["file.tar"] = []byte("data")

	ttl := 100 * time.Millisecond
	cache := newTestCache(t, store, 0, ttl)

	// Cache it.
	_, err := cache.Get(context.Background(), "file.tar")
	require.NoError(t, err)
	assert.True(t, cache.Exists("file.tar"))

	// Wait for TTL to expire.
	time.Sleep(200 * time.Millisecond)

	// Exists should return false after TTL expiry.
	assert.False(t, cache.Exists("file.tar"))
}

func TestCacheGetNotFound(t *testing.T) {
	store := newMockObjStore()
	cache := newTestCache(t, store, 0, 0)

	_, err := cache.Get(context.Background(), "nonexistent.tar")
	assert.Error(t, err)
}

func TestCacheInvalidateNonexistent(t *testing.T) {
	store := newMockObjStore()
	cache := newTestCache(t, store, 0, 0)

	// Invalidating a non-existent key should not error.
	err := cache.Invalidate("nonexistent.tar")
	assert.NoError(t, err)
}
