package gcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"

	"github.com/vyprai/loka/internal/objstore"
)

// Store implements objstore.ObjectStore using Google Cloud Storage.
type Store struct {
	client *storage.Client
}

// New creates a new GCS object store.
// Uses Application Default Credentials (ADC) from the environment.
func New(ctx context.Context) (*Store, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create gcs client: %w", err)
	}
	return &Store{client: client}, nil
}

// Close closes the underlying GCS client.
func (s *Store) Close() error {
	return s.client.Close()
}

func (s *Store) Put(ctx context.Context, bucket, key string, reader io.Reader, _ int64) error {
	w := s.client.Bucket(bucket).Object(key).NewWriter(ctx)
	if _, err := io.Copy(w, reader); err != nil {
		w.Close()
		return fmt.Errorf("gcs write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("gcs close writer: %w", err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	r, err := s.client.Bucket(bucket).Object(key).NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcs read: %w", err)
	}
	return r, nil
}

func (s *Store) Delete(ctx context.Context, bucket, key string) error {
	err := s.client.Bucket(bucket).Object(key).Delete(ctx)
	if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("gcs delete: %w", err)
	}
	return nil
}

func (s *Store) Exists(ctx context.Context, bucket, key string) (bool, error) {
	_, err := s.client.Bucket(bucket).Object(key).Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("gcs attrs: %w", err)
	}
	return true, nil
}

func (s *Store) GetPresignedURL(ctx context.Context, bucket, key string, expiry time.Duration) (string, error) {
	url, err := s.client.Bucket(bucket).SignedURL(key, &storage.SignedURLOptions{
		Method:  "GET",
		Expires: time.Now().Add(expiry),
	})
	if err != nil {
		return "", fmt.Errorf("gcs signed url: %w", err)
	}
	return url, nil
}

func (s *Store) List(ctx context.Context, bucket, prefix string) ([]objstore.ObjectInfo, error) {
	var objects []objstore.ObjectInfo
	it := s.client.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: prefix})
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("gcs list: %w", err)
		}
		objects = append(objects, objstore.ObjectInfo{
			Key:          attrs.Name,
			Size:         attrs.Size,
			LastModified: attrs.Updated,
			ETag:         attrs.Etag,
		})
	}
	return objects, nil
}

var _ objstore.ObjectStore = (*Store)(nil)
