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

// failStore is a mock ObjectStore that fails N times then succeeds.
type failStore struct {
	failCount int
	calls     int
	data      map[string]string // bucket/key → content
}

func (f *failStore) Put(ctx context.Context, bucket, key string, reader io.Reader, size int64) error {
	f.calls++
	if f.calls <= f.failCount {
		return fmt.Errorf("transient error %d", f.calls)
	}
	data, _ := io.ReadAll(reader)
	if f.data == nil {
		f.data = make(map[string]string)
	}
	f.data[bucket+"/"+key] = string(data)
	return nil
}
func (f *failStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	f.calls++
	if f.calls <= f.failCount {
		return nil, fmt.Errorf("transient error %d", f.calls)
	}
	v, ok := f.data[bucket+"/"+key]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return io.NopCloser(strings.NewReader(v)), nil
}
func (f *failStore) Delete(ctx context.Context, bucket, key string) error {
	f.calls++
	if f.calls <= f.failCount {
		return fmt.Errorf("transient error %d", f.calls)
	}
	delete(f.data, bucket+"/"+key)
	return nil
}
func (f *failStore) Exists(ctx context.Context, bucket, key string) (bool, error) {
	f.calls++
	if f.calls <= f.failCount {
		return false, fmt.Errorf("transient error %d", f.calls)
	}
	_, ok := f.data[bucket+"/"+key]
	return ok, nil
}
func (f *failStore) GetPresignedURL(ctx context.Context, bucket, key string, expiry time.Duration) (string, error) {
	f.calls++
	if f.calls <= f.failCount {
		return "", fmt.Errorf("transient error %d", f.calls)
	}
	return "https://presigned/" + key, nil
}
func (f *failStore) List(ctx context.Context, bucket, prefix string) ([]ObjectInfo, error) {
	f.calls++
	if f.calls <= f.failCount {
		return nil, fmt.Errorf("transient error %d", f.calls)
	}
	return []ObjectInfo{{Key: "test"}}, nil
}

func TestRetryStore_Put_RetriesOnFailure(t *testing.T) {
	inner := &failStore{failCount: 2, data: make(map[string]string)}
	store := NewRetryStore(inner)

	err := store.Put(context.Background(), "b", "k", strings.NewReader("data"), 4)
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if inner.data["b/k"] != "data" {
		t.Error("expected data to be stored")
	}
	if inner.calls != 3 { // 2 failures + 1 success
		t.Errorf("expected 3 calls, got %d", inner.calls)
	}
}

func TestRetryStore_Put_ExceedsMaxRetries(t *testing.T) {
	inner := &failStore{failCount: 10} // Always fails
	store := NewRetryStore(inner)
	store.backoff = 1 * time.Millisecond // Speed up test

	err := store.Put(context.Background(), "b", "k", strings.NewReader("data"), 4)
	if err == nil {
		t.Fatal("expected error after max retries")
	}
}

func TestRetryStore_Get_EventuallySucceeds(t *testing.T) {
	inner := &failStore{failCount: 1, data: map[string]string{"b/k": "hello"}}
	store := NewRetryStore(inner)
	store.backoff = 1 * time.Millisecond

	rc, err := store.Get(context.Background(), "b", "k")
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "hello" {
		t.Errorf("expected 'hello', got %q", string(data))
	}
}

func TestRetryStore_Delete_RetriesOnFailure(t *testing.T) {
	inner := &failStore{failCount: 1, data: map[string]string{"b/k": "x"}}
	store := NewRetryStore(inner)
	store.backoff = 1 * time.Millisecond

	err := store.Delete(context.Background(), "b", "k")
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

func TestRetryStore_Exists_RetriesOnFailure(t *testing.T) {
	inner := &failStore{failCount: 1, data: map[string]string{"b/k": "x"}}
	store := NewRetryStore(inner)
	store.backoff = 1 * time.Millisecond

	exists, err := store.Exists(context.Background(), "b", "k")
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if !exists {
		t.Error("expected exists=true")
	}
}

func TestRetryStore_ContextCancellation(t *testing.T) {
	inner := &failStore{failCount: 10}
	store := NewRetryStore(inner)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	err := store.Put(ctx, "b", "k", strings.NewReader("data"), 4)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

func TestRetryStore_List_SingleRetry(t *testing.T) {
	inner := &failStore{failCount: 1}
	store := NewRetryStore(inner)
	store.backoff = 1 * time.Millisecond

	items, err := store.List(context.Background(), "b", "prefix")
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 item, got %d", len(items))
	}
}

func TestRetryStore_GetPresignedURL_RetriesOnFailure(t *testing.T) {
	inner := &failStore{failCount: 1}
	store := NewRetryStore(inner)
	store.backoff = 1 * time.Millisecond

	url, err := store.GetPresignedURL(context.Background(), "b", "k", time.Hour)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if url == "" {
		t.Error("expected non-empty URL")
	}
}

func TestRetryStore_List_ExceedsMaxRetries(t *testing.T) {
	inner := &failStore{failCount: 10}
	store := NewRetryStore(inner)
	store.backoff = 1 * time.Millisecond

	_, err := store.List(context.Background(), "b", "prefix")
	if err == nil {
		t.Fatal("expected error after max retries for List")
	}
}

func TestRetryStore_Put_NilReader(t *testing.T) {
	inner := &failStore{data: make(map[string]string)}
	store := NewRetryStore(inner)
	// Nil reader panics in io.ReadAll — document this is expected.
	// Callers must never pass nil reader.
	defer func() {
		if r := recover(); r != nil {
			t.Logf("Put with nil reader panics as expected: %v", r)
		}
	}()
	store.Put(context.Background(), "b", "k", nil, 0)
	// If we reach here, it didn't panic (some stores may handle nil).
}

// Ensure the bytes import is used by the test package.
var _ = bytes.NewReader
