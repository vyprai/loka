package objstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

type failNStore struct {
	failCount int
	calls     int
	lastData  []byte
}

func (f *failNStore) Put(_ context.Context, _, _ string, r io.Reader, _ int64) error {
	f.calls++
	data, _ := io.ReadAll(r)
	f.lastData = data
	if f.calls <= f.failCount {
		return fmt.Errorf("transient error %d", f.calls)
	}
	return nil
}

func (f *failNStore) Get(context.Context, string, string) (io.ReadCloser, error) { return nil, nil }
func (f *failNStore) Delete(context.Context, string, string) error               { return nil }
func (f *failNStore) Exists(context.Context, string, string) (bool, error)       { return false, nil }
func (f *failNStore) GetPresignedURL(context.Context, string, string, time.Duration) (string, error) {
	return "", nil
}
func (f *failNStore) List(context.Context, string, string) ([]ObjectInfo, error) { return nil, nil }

func TestRetryPut_SeekableReader_RewindsOnRetry(t *testing.T) {
	inner := &failNStore{failCount: 2}
	store := &RetryStore{inner: inner, maxRetries: 3, backoff: 0}

	data := []byte("hello world")
	reader := bytes.NewReader(data)

	err := store.Put(context.Background(), "bucket", "key", reader, int64(len(data)))
	if err != nil {
		t.Fatalf("Put should succeed after retries: %v", err)
	}
	if !bytes.Equal(inner.lastData, data) {
		t.Errorf("expected %q, got %q — reader not rewound correctly", data, inner.lastData)
	}
	if inner.calls != 3 {
		t.Errorf("expected 3 calls (2 failures + 1 success), got %d", inner.calls)
	}
}

func TestRetryPut_NonSeekableReader_FailsFastOnRetry(t *testing.T) {
	inner := &failNStore{failCount: 1}
	store := &RetryStore{inner: inner, maxRetries: 3, backoff: 0}

	// NopCloser wraps but doesn't expose Seek.
	nonSeekable := io.NopCloser(strings.NewReader("data"))

	err := store.Put(context.Background(), "bucket", "key", nonSeekable, 4)
	if err == nil {
		t.Fatal("Put with non-seekable reader should fail after first error")
	}
	if inner.calls != 1 {
		t.Errorf("expected 1 call, got %d", inner.calls)
	}
}

func TestRetryPut_SeekFailure_ReturnsLastError(t *testing.T) {
	inner := &failNStore{failCount: 1}
	store := &RetryStore{inner: inner, maxRetries: 3, backoff: 0}

	reader := &failSeeker{data: []byte("test"), seekErr: fmt.Errorf("seek broken")}
	err := store.Put(context.Background(), "bucket", "key", reader, 4)
	if err == nil {
		t.Fatal("Put should fail when Seek fails")
	}
}

type failSeeker struct {
	data    []byte
	pos     int
	seekErr error
}

func (f *failSeeker) Read(p []byte) (int, error) {
	if f.pos >= len(f.data) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.pos:])
	f.pos += n
	return n, nil
}

func (f *failSeeker) Seek(offset int64, whence int) (int64, error) {
	if f.seekErr != nil {
		return 0, f.seekErr
	}
	f.pos = int(offset)
	return int64(f.pos), nil
}

func TestRetryPut_ContextCancelled_NoRetry(t *testing.T) {
	inner := &failNStore{failCount: 5}
	store := &RetryStore{inner: inner, maxRetries: 5, backoff: 0}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := store.Put(ctx, "bucket", "key", bytes.NewReader([]byte("x")), 1)
	if err == nil {
		t.Fatal("expected error with cancelled context")
	}
	if inner.calls > 1 {
		t.Errorf("should not retry on cancelled context, got %d calls", inner.calls)
	}
}

func TestRetryPut_LargeSeekableFile_CorrectDataOnRetry(t *testing.T) {
	inner := &failNStore{failCount: 1}
	store := &RetryStore{inner: inner, maxRetries: 3, backoff: 0}

	// 1MB data.
	data := bytes.Repeat([]byte("abcdefghijklmnop"), 65536)
	reader := bytes.NewReader(data)

	err := store.Put(context.Background(), "bucket", "key", reader, int64(len(data)))
	if err != nil {
		t.Fatalf("Put should succeed after retry: %v", err)
	}
	if !bytes.Equal(inner.lastData, data) {
		t.Errorf("data mismatch after retry — seek did not rewind correctly (got %d bytes, want %d)", len(inner.lastData), len(data))
	}
}
