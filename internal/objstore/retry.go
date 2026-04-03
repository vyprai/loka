package objstore

import (
	"context"
	"io"
	"math/rand/v2"
	"time"
)

// RetryStore wraps an ObjectStore with automatic retry and exponential backoff
// with jitter for transient errors (timeouts, temporary failures).
type RetryStore struct {
	inner      ObjectStore
	maxRetries int
	backoff    time.Duration
}

// NewRetryStore wraps an ObjectStore with retry logic.
// Default: 3 retries with 1s initial backoff (doubles each retry + jitter).
func NewRetryStore(inner ObjectStore) *RetryStore {
	return &RetryStore{
		inner:      inner,
		maxRetries: 3,
		backoff:    1 * time.Second,
	}
}

func (r *RetryStore) Put(ctx context.Context, bucket, key string, reader io.Reader, size int64) error {
	var lastErr error
	for i := 0; i <= r.maxRetries; i++ {
		// If reader supports Seek, rewind before retry to avoid partial uploads.
		if i > 0 {
			if seeker, ok := reader.(io.Seeker); ok {
				if _, err := seeker.Seek(0, io.SeekStart); err != nil {
					return lastErr // Can't rewind — return last error.
				}
			} else {
				return lastErr // Non-seekable reader consumed — can't retry safely.
			}
		}

		if err := r.inner.Put(ctx, bucket, key, reader, size); err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return err // Context cancelled, don't retry.
			}
			r.sleep(ctx, i)
			continue
		}
		return nil
	}
	return lastErr
}

func (r *RetryStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	var lastErr error
	for i := 0; i <= r.maxRetries; i++ {
		rc, err := r.inner.Get(ctx, bucket, key)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, err
			}
			r.sleep(ctx, i)
			continue
		}
		return rc, nil
	}
	return nil, lastErr
}

func (r *RetryStore) Delete(ctx context.Context, bucket, key string) error {
	var lastErr error
	for i := 0; i <= r.maxRetries; i++ {
		if err := r.inner.Delete(ctx, bucket, key); err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return err
			}
			r.sleep(ctx, i)
			continue
		}
		return nil
	}
	return lastErr
}

func (r *RetryStore) Exists(ctx context.Context, bucket, key string) (bool, error) {
	var lastErr error
	for i := 0; i <= r.maxRetries; i++ {
		exists, err := r.inner.Exists(ctx, bucket, key)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return false, err
			}
			r.sleep(ctx, i)
			continue
		}
		return exists, nil
	}
	return false, lastErr
}

func (r *RetryStore) GetPresignedURL(ctx context.Context, bucket, key string, expiry time.Duration) (string, error) {
	var lastErr error
	for i := 0; i <= r.maxRetries; i++ {
		url, err := r.inner.GetPresignedURL(ctx, bucket, key, expiry)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return "", err
			}
			r.sleep(ctx, i)
			continue
		}
		return url, nil
	}
	return "", lastErr
}

func (r *RetryStore) List(ctx context.Context, bucket, prefix string) ([]ObjectInfo, error) {
	items, err := r.inner.List(ctx, bucket, prefix)
	if err != nil && ctx.Err() == nil {
		r.sleep(ctx, 0)
		return r.inner.List(ctx, bucket, prefix)
	}
	return items, err
}

func (r *RetryStore) sleep(ctx context.Context, attempt int) {
	d := r.backoff
	for i := 0; i < attempt; i++ {
		d *= 2
	}
	if d <= 0 {
		return // No sleep needed (e.g., in tests with backoff=0).
	}
	// Add jitter: ±25% to prevent thundering herd.
	jitter := time.Duration(rand.Int64N(max(int64(d/2), 1)))
	d = d - d/4 + jitter

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
