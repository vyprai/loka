package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vyprai/loka/internal/objstore"
)

const (
	// DefaultMaxCacheSize is the default max cache size (10 GB).
	DefaultMaxCacheSize int64 = 10 * 1024 * 1024 * 1024
	// DefaultCacheTTL is the default time-to-live for cached files.
	DefaultCacheTTL = 24 * time.Hour
	// DefaultMinFreeSpace is the minimum free disk space before emergency eviction (2 GB).
	DefaultMinFreeSpace int64 = 2 * 1024 * 1024 * 1024
)

// CacheStats holds statistics about the cache.
type CacheStats struct {
	TotalSize  int64 `json:"total_size"`
	LayerCount int   `json:"layer_count"`
	PackCount  int   `json:"pack_count"`
	SnapCount  int   `json:"snap_count"`
	HitCount   int64 `json:"hit_count"`
	MissCount  int64 `json:"miss_count"`
	EvictCount int64 `json:"evict_count"`
}

// Cache provides a local filesystem cache backed by an object store.
// Files are downloaded on first access and served from cache thereafter.
// LRU eviction keeps total size under maxSize, and files older than ttl
// are re-downloaded on next access.
//
// Layer-aware features:
//   - Reference counting tracks which images use which layers.
//   - Disk pressure monitoring triggers emergency eviction.
//   - Priority-based eviction: packs > snapshots > unreferenced layers.
//   - Background sweep goroutine for periodic cleanup.
type Cache struct {
	cacheDir     string
	objStore     objstore.ObjectStore
	bucket       string
	maxSize      int64         // max cache size in bytes (0 = unlimited)
	ttl          time.Duration // max age before re-download (0 = no expiry)
	minFreeSpace int64         // minimum free disk space before emergency eviction
	logger       *slog.Logger
	mu           sync.Mutex

	// Reference counting: layerKey -> set of imageIDs referencing it.
	refs   map[string]map[string]bool
	refsMu sync.Mutex

	// Stats counters (atomic for lock-free reads).
	hitCount   atomic.Int64
	missCount  atomic.Int64
	evictCount atomic.Int64
}

// NewCache creates a new cache backed by the given object store.
func NewCache(cacheDir string, objStore objstore.ObjectStore, bucket string, logger *slog.Logger) *Cache {
	return &Cache{
		cacheDir:     cacheDir,
		objStore:     objStore,
		bucket:       bucket,
		maxSize:      DefaultMaxCacheSize,
		ttl:          DefaultCacheTTL,
		minFreeSpace: DefaultMinFreeSpace,
		logger:       logger,
		refs:         make(map[string]map[string]bool),
	}
}

// NewCacheWithOptions creates a new cache with explicit size limit and TTL.
// A maxSize of 0 means unlimited; a ttl of 0 means no expiry.
func NewCacheWithOptions(cacheDir string, objStore objstore.ObjectStore, bucket string, maxSize int64, ttl time.Duration, logger *slog.Logger) *Cache {
	return &Cache{
		cacheDir:     cacheDir,
		objStore:     objStore,
		bucket:       bucket,
		maxSize:      maxSize,
		ttl:          ttl,
		minFreeSpace: DefaultMinFreeSpace,
		logger:       logger,
		refs:         make(map[string]map[string]bool),
	}
}

// SetMinFreeSpace sets the minimum free disk space threshold for emergency eviction.
func (c *Cache) SetMinFreeSpace(bytes int64) {
	c.minFreeSpace = bytes
}

// Get returns the local filesystem path for the given object store key.
// Downloads from objstore on cache miss or if the cached file has expired.
func (c *Cache) Get(ctx context.Context, key string) (string, error) {
	localPath := c.localPath(key)

	// Check cache hit with TTL.
	if info, err := os.Stat(localPath); err == nil {
		if c.ttl <= 0 || time.Since(info.ModTime()) < c.ttl {
			c.logger.Debug("cache hit", "key", key, "path", localPath)
			c.hitCount.Add(1)
			return localPath, nil
		}
		// Expired — remove and re-download.
		c.logger.Info("cache expired, re-downloading", "key", key, "age", time.Since(info.ModTime()))
		os.Remove(localPath)
	}

	c.logger.Info("cache miss, downloading", "key", key, "bucket", c.bucket)
	c.missCount.Add(1)

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

// ── Reference Counting ──────────────────────────────────

// AddRef records that imageID references layerKey.
func (c *Cache) AddRef(layerKey, imageID string) {
	c.refsMu.Lock()
	defer c.refsMu.Unlock()
	if c.refs[layerKey] == nil {
		c.refs[layerKey] = make(map[string]bool)
	}
	c.refs[layerKey][imageID] = true
}

// RemoveRef removes the reference from imageID to layerKey.
func (c *Cache) RemoveRef(layerKey, imageID string) {
	c.refsMu.Lock()
	defer c.refsMu.Unlock()
	if refs, ok := c.refs[layerKey]; ok {
		delete(refs, imageID)
		if len(refs) == 0 {
			delete(c.refs, layerKey)
		}
	}
}

// RefCount returns the number of images referencing layerKey.
func (c *Cache) RefCount(layerKey string) int {
	c.refsMu.Lock()
	defer c.refsMu.Unlock()
	return len(c.refs[layerKey])
}

// ── Disk Pressure Monitoring ────────────────────────────

// checkDiskPressure checks available disk space and emergency evicts if low.
func (c *Cache) checkDiskPressure() {
	freeBytes := diskFreeSpace(c.cacheDir)
	if freeBytes < 0 {
		return // Platform does not support disk space check.
	}
	if freeBytes < c.minFreeSpace {
		c.logger.Warn("disk pressure detected, emergency eviction",
			"free_bytes", freeBytes, "min_free", c.minFreeSpace)
		c.evictUntilFree(c.minFreeSpace)
	}
}

// evictUntilFree evicts cached files until at least targetFree bytes are available.
func (c *Cache) evictUntilFree(targetFree int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries := c.collectEntries()
	c.sortByEvictionPriority(entries)

	for _, e := range entries {
		freeBytes := diskFreeSpace(c.cacheDir)
		if freeBytes >= targetFree {
			break
		}
		if c.isReferencedLayer(e.path) {
			continue
		}
		if err := os.Remove(e.path); err != nil {
			c.logger.Warn("emergency evict failed", "path", e.path, "error", err)
			continue
		}
		c.logger.Info("emergency evicted", "path", e.path, "size", e.size)
		c.evictCount.Add(1)
	}
}

// ── Background Sweep ────────────────────────────────────

// StartSweep starts a background goroutine that periodically checks disk
// pressure and evicts expired entries.
func (c *Cache) StartSweep(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.checkDiskPressure()
				c.evictExpired()
			}
		}
	}()
}

// evictExpired removes all cached files that have exceeded their TTL.
func (c *Cache) evictExpired() {
	if c.ttl <= 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	filepath.Walk(c.cacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if now.Sub(info.ModTime()) >= c.ttl {
			if removeErr := os.Remove(path); removeErr == nil {
				c.logger.Debug("sweep evicted expired", "path", path,
					"age", now.Sub(info.ModTime()))
				c.evictCount.Add(1)
			}
		}
		return nil
	})
}

// ── Priority-Based Eviction ─────────────────────────────

// evictByPriority evicts entries in priority order:
//  1. Layer-packs (cheap to rebuild from individual layers)
//  2. Warm snapshots (cheap to recreate)
//  3. Individual layers with no references
//  4. Never evict layers with RefCount > 0
func (c *Cache) evictByPriority() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.maxSize <= 0 {
		return
	}

	entries := c.collectEntries()
	totalSize := c.sumSize(entries)
	if totalSize <= c.maxSize {
		return
	}

	c.sortByEvictionPriority(entries)

	for _, e := range entries {
		if totalSize <= c.maxSize {
			break
		}
		if c.isReferencedLayer(e.path) {
			continue
		}
		if err := os.Remove(e.path); err != nil {
			c.logger.Warn("priority evict failed", "path", e.path, "error", err)
			continue
		}
		c.logger.Info("priority evicted", "path", e.path, "size", e.size,
			"priority", e.priority)
		totalSize -= e.size
		c.evictCount.Add(1)
	}
}

// cacheEntryPriority assigns eviction priority (lower = evict first).
const (
	priorityPack    = 0 // Layer-packs: cheap to rebuild.
	prioritySnap    = 1 // Warm snapshots: cheap to recreate.
	priorityLayer   = 2 // Individual layers: most expensive.
	priorityUnknown = 3 // Unknown files.
)

// prioritizedEntry extends cacheEntry with eviction priority.
type prioritizedEntry struct {
	cacheEntry
	priority int
}

// classifyEntry determines the eviction priority of a cache file based on
// its path. Layer-packs and snapshots are cheap to recreate; individual
// layers are the most expensive.
func classifyEntry(path string) int {
	rel := filepath.Base(path)
	dir := filepath.Dir(path)

	switch {
	case strings.Contains(rel, "layer-pack"):
		return priorityPack
	case strings.Contains(rel, "snapshot") || strings.Contains(dir, "snapshot"):
		return prioritySnap
	case strings.Contains(dir, "sha256:") || strings.HasSuffix(rel, "layer.tar"):
		return priorityLayer
	default:
		return priorityUnknown
	}
}

func (c *Cache) collectEntries() []prioritizedEntry {
	var entries []prioritizedEntry
	filepath.Walk(c.cacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		entries = append(entries, prioritizedEntry{
			cacheEntry: cacheEntry{
				path:    path,
				size:    info.Size(),
				modTime: info.ModTime(),
			},
			priority: classifyEntry(path),
		})
		return nil
	})
	return entries
}

func (c *Cache) sortByEvictionPriority(entries []prioritizedEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].priority != entries[j].priority {
			return entries[i].priority < entries[j].priority // Lower priority = evict first.
		}
		return entries[i].modTime.Before(entries[j].modTime) // Oldest first within same priority.
	})
}

func (c *Cache) sumSize(entries []prioritizedEntry) int64 {
	var total int64
	for _, e := range entries {
		total += e.size
	}
	return total
}

// isReferencedLayer checks if a cache path corresponds to a layer with active references.
func (c *Cache) isReferencedLayer(path string) bool {
	// Extract a potential layer key from the path.
	rel, err := filepath.Rel(c.cacheDir, path)
	if err != nil {
		return false
	}
	c.refsMu.Lock()
	defer c.refsMu.Unlock()
	return len(c.refs[rel]) > 0
}

// ── Cache Stats ─────────────────────────────────────────

// Stats returns current cache statistics.
func (c *Cache) Stats() CacheStats {
	stats := CacheStats{
		HitCount:   c.hitCount.Load(),
		MissCount:  c.missCount.Load(),
		EvictCount: c.evictCount.Load(),
	}

	filepath.Walk(c.cacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		stats.TotalSize += info.Size()
		switch classifyEntry(path) {
		case priorityLayer:
			stats.LayerCount++
		case priorityPack:
			stats.PackCount++
		case prioritySnap:
			stats.SnapCount++
		}
		return nil
	})

	return stats
}

// ── Clean Methods ───────────────────────────────────────

// Clean evicts expired entries and runs priority-based eviction if over limit.
func (c *Cache) Clean() {
	c.evictExpired()
	c.evictByPriority()
}

// CleanAll removes all cached files.
func (c *Cache) CleanAll() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries, err := os.ReadDir(c.cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read cache dir: %w", err)
	}

	var count int
	for _, e := range entries {
		path := filepath.Join(c.cacheDir, e.Name())
		if err := os.RemoveAll(path); err != nil {
			c.logger.Warn("clean all: failed to remove", "path", path, "error", err)
			continue
		}
		count++
	}

	c.logger.Info("cache cleaned", "entries_removed", count)
	return nil
}

func (c *Cache) localPath(key string) string {
	return filepath.Join(c.cacheDir, key)
}

// CacheDir returns the cache directory path.
func (c *Cache) CacheDir() string {
	return c.cacheDir
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
		c.evictCount.Add(1)
	}
}
