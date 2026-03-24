package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/vyprai/loka/internal/objstore"
)

// Cache provides a local filesystem cache backed by an object store.
// Files are downloaded on first access and served from cache thereafter.
type Cache struct {
	cacheDir string
	objStore objstore.ObjectStore
	bucket   string
	logger   *slog.Logger
}

// NewCache creates a new cache backed by the given object store.
func NewCache(cacheDir string, objStore objstore.ObjectStore, bucket string, logger *slog.Logger) *Cache {
	return &Cache{
		cacheDir: cacheDir,
		objStore: objStore,
		bucket:   bucket,
		logger:   logger,
	}
}

// Get returns the local filesystem path for the given object store key.
// Downloads from objstore on cache miss.
func (c *Cache) Get(ctx context.Context, key string) (string, error) {
	localPath := filepath.Join(c.cacheDir, key)

	if _, err := os.Stat(localPath); err == nil {
		c.logger.Debug("cache hit", "key", key, "path", localPath)
		return localPath, nil
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
	return localPath, nil
}

// Exists checks if a key is in the local cache.
func (c *Cache) Exists(key string) bool {
	localPath := filepath.Join(c.cacheDir, key)
	_, err := os.Stat(localPath)
	return err == nil
}

// Invalidate removes a key from the local cache.
func (c *Cache) Invalidate(key string) error {
	localPath := filepath.Join(c.cacheDir, key)
	if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("invalidate %s: %w", key, err)
	}
	return nil
}
