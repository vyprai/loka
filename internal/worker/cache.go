package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/vyprai/loka/internal/objstore"
)

const (
	// DefaultMaxCacheSize is the default max cache size (10 GB).
	DefaultMaxCacheSize int64 = 10 * 1024 * 1024 * 1024
	// DefaultCacheTTL is the default time-to-live for cached files.
	DefaultCacheTTL = 24 * time.Hour
)

// Cache provides a local filesystem cache backed by an object store.
// Files are downloaded on first access and served from cache thereafter.
// LRU eviction keeps total size under maxSize, and files older than ttl
// are re-downloaded on next access.
type Cache struct {
	cacheDir string
	objStore objstore.ObjectStore
	bucket   string
	maxSize  int64         // max cache size in bytes (0 = unlimited)
	ttl      time.Duration // max age before re-download (0 = no expiry)
	logger   *slog.Logger
	mu       sync.Mutex
}

// NewCache creates a new cache backed by the given object store.
func NewCache(cacheDir string, objStore objstore.ObjectStore, bucket string, logger *slog.Logger) *Cache {
	return &Cache{
		cacheDir: cacheDir,
		objStore: objStore,
		bucket:   bucket,
		maxSize:  DefaultMaxCacheSize,
		ttl:      DefaultCacheTTL,
		logger:   logger,
	}
}

// NewCacheWithOptions creates a new cache with explicit size limit and TTL.
// A maxSize of 0 means unlimited; a ttl of 0 means no expiry.
func NewCacheWithOptions(cacheDir string, objStore objstore.ObjectStore, bucket string, maxSize int64, ttl time.Duration, logger *slog.Logger) *Cache {
	return &Cache{
		cacheDir: cacheDir,
		objStore: objStore,
		bucket:   bucket,
		maxSize:  maxSize,
		ttl:      ttl,
		logger:   logger,
	}
}

// Get returns the local filesystem path for the given object store key.
// Downloads from objstore on cache miss or if the cached file has expired.
func (c *Cache) Get(ctx context.Context, key string) (string, error) {
	localPath := c.localPath(key)

	// Check cache hit with TTL.
	if info, err := os.Stat(localPath); err == nil {
		if c.ttl <= 0 || time.Since(info.ModTime()) < c.ttl {
			c.logger.Debug("cache hit", "key", key, "path", localPath)
			return localPath, nil
		}
		// Expired — remove and re-download.
		c.logger.Info("cache expired, re-downloading", "key", key, "age", time.Since(info.ModTime()))
		os.Remove(localPath)
	}

	c.logger.Info("cache miss, downloading", "key", key, "bucket", c.bucket)

	reader, err := c.objStore.Get(ctx, c.bucket, key)
	if err != nil {
		return "", fmt.Errorf("download %s/%s: %w", c.bucket, key, err)
	}
	defer reader.Close()

	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}

	f, err := os.Create(localPath)
	if err != nil {
		return "", fmt.Errorf("create cache file: %w", err)
	}
	defer f.Close()

	if _, err := f.ReadFrom(reader); err != nil {
		os.Remove(localPath) // Clean up partial download.
		return "", fmt.Errorf("write cache file: %w", err)
	}

	c.logger.Info("cached", "key", key, "path", localPath)

	// Evict oldest files if over size limit.
	c.evictIfNeeded()

	return localPath, nil
}

// Exists checks if a key is in the local cache (and not expired).
func (c *Cache) Exists(key string) bool {
	localPath := c.localPath(key)
	info, err := os.Stat(localPath)
	if err != nil {
		return false
	}
	if c.ttl > 0 && time.Since(info.ModTime()) >= c.ttl {
		return false
	}
	return true
}

// Invalidate removes a key from the local cache.
func (c *Cache) Invalidate(key string) error {
	localPath := c.localPath(key)
	if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("invalidate %s: %w", key, err)
	}
	return nil
}

func (c *Cache) localPath(key string) string {
	return filepath.Join(c.cacheDir, key)
}

// cacheEntry holds metadata about a single cached file.
type cacheEntry struct {
	path    string
	size    int64
	modTime time.Time
}

// cacheSize walks the cache directory and returns the total size in bytes.
func (c *Cache) cacheSize() int64 {
	var total int64
	filepath.Walk(c.cacheDir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

// evictIfNeeded removes the oldest files until total cache size is under maxSize.
func (c *Cache) evictIfNeeded() {
	if c.maxSize <= 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Collect all cached files.
	var entries []cacheEntry
	var totalSize int64
	filepath.Walk(c.cacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		entries = append(entries, cacheEntry{
			path:    path,
			size:    info.Size(),
			modTime: info.ModTime(),
		})
		totalSize += info.Size()
		return nil
	})

	if totalSize <= c.maxSize {
		return
	}

	// Sort oldest first.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].modTime.Before(entries[j].modTime)
	})

	// Remove oldest files until under the limit.
	for _, e := range entries {
		if totalSize <= c.maxSize {
			break
		}
		if err := os.Remove(e.path); err != nil {
			c.logger.Warn("cache evict failed", "path", e.path, "error", err)
			continue
		}
		c.logger.Info("cache evicted", "path", e.path, "size", e.size, "age", time.Since(e.modTime))
		totalSize -= e.size
	}
}
