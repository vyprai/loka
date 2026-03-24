package local

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/vyprai/loka/internal/objstore"
)

// Store implements objstore.ObjectStore using the local filesystem.
// Buckets are top-level directories under the root path.
type Store struct {
	root string
}

// New creates a new local filesystem object store.
func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create objstore root: %w", err)
	}
	return &Store{root: root}, nil
}

func (s *Store) path(bucket, key string) string {
	return filepath.Join(s.root, bucket, key)
}

func (s *Store) Put(_ context.Context, bucket, key string, reader io.Reader, _ int64) error {
	p := s.path(bucket, key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	f, err := os.Create(p)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()
	if _, err := io.Copy(f, reader); err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	return nil
}

func (s *Store) Get(_ context.Context, bucket, key string) (io.ReadCloser, error) {
	f, err := os.Open(s.path(bucket, key))
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	return f, nil
}

func (s *Store) Delete(_ context.Context, bucket, key string) error {
	return os.Remove(s.path(bucket, key))
}

func (s *Store) Exists(_ context.Context, bucket, key string) (bool, error) {
	_, err := os.Stat(s.path(bucket, key))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (s *Store) GetPresignedURL(_ context.Context, bucket, key string, _ time.Duration) (string, error) {
	// Local filesystem doesn't support presigned URLs.
	// Return a file:// URL for local development.
	return "file://" + s.path(bucket, key), nil
}

func (s *Store) List(_ context.Context, bucket, prefix string) ([]objstore.ObjectInfo, error) {
	const maxResults = 10000

	dir := filepath.Join(s.root, bucket, prefix)
	var objects []objstore.ObjectInfo

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip inaccessible paths.
		}
		if info.IsDir() {
			return nil
		}
		if len(objects) >= maxResults {
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(filepath.Join(s.root, bucket), path)
		objects = append(objects, objstore.ObjectInfo{
			Key:          rel,
			Size:         info.Size(),
			LastModified: info.ModTime(),
		})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("list objects: %w", err)
	}
	if len(objects) >= maxResults {
		fmt.Fprintf(os.Stderr, "warning: objstore local List(%s/%s) truncated at %d results\n", bucket, prefix, maxResults)
	}
	return objects, nil
}

var _ objstore.ObjectStore = (*Store)(nil)
