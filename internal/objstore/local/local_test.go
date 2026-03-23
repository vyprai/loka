package local

import (
	"bytes"
	"context"
	"io"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestPutGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	data := []byte("hello, object store")
	err := s.Put(ctx, "test-bucket", "greeting.txt", bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, err := s.Get(ctx, "test-bucket", "greeting.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("round trip mismatch: got %q, want %q", got, data)
	}
}

func TestDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	data := []byte("to be deleted")
	if err := s.Put(ctx, "bucket", "delete-me.txt", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Verify it exists first.
	exists, err := s.Exists(ctx, "bucket", "delete-me.txt")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Fatal("expected object to exist before deletion")
	}

	// Delete it.
	if err := s.Delete(ctx, "bucket", "delete-me.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Verify it no longer exists.
	exists, err = s.Exists(ctx, "bucket", "delete-me.txt")
	if err != nil {
		t.Fatalf("Exists after delete: %v", err)
	}
	if exists {
		t.Error("expected object to not exist after deletion")
	}
}

func TestExists_Found(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	data := []byte("exists")
	if err := s.Put(ctx, "bucket", "key", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	exists, err := s.Exists(ctx, "bucket", "key")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Error("expected object to exist")
	}
}

func TestExists_NotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	exists, err := s.Exists(ctx, "bucket", "nonexistent-key")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		t.Error("expected object to not exist")
	}
}

func TestList_Empty(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	objects, err := s.List(ctx, "empty-bucket", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 0 {
		t.Errorf("expected 0 objects, got %d", len(objects))
	}
}

func TestList_MultipleObjects(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	files := map[string]string{
		"a.txt": "content a",
		"b.txt": "content b",
		"c.txt": "content c",
	}
	for key, content := range files {
		data := []byte(content)
		if err := s.Put(ctx, "bucket", key, bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
	}

	objects, err := s.List(ctx, "bucket", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 3 {
		t.Errorf("expected 3 objects, got %d", len(objects))
	}
}

func TestList_WithPrefix(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Create files under different prefixes.
	puts := []struct {
		key     string
		content string
	}{
		{"data/train/a.csv", "train a"},
		{"data/train/b.csv", "train b"},
		{"data/test/c.csv", "test c"},
		{"models/model.bin", "model"},
	}
	for _, p := range puts {
		data := []byte(p.content)
		if err := s.Put(ctx, "bucket", p.key, bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatalf("Put %s: %v", p.key, err)
		}
	}

	// List only under data/train.
	objects, err := s.List(ctx, "bucket", "data/train")
	if err != nil {
		t.Fatalf("List with prefix: %v", err)
	}
	if len(objects) != 2 {
		t.Errorf("expected 2 objects under data/train, got %d", len(objects))
	}
	for _, obj := range objects {
		if obj.Key != "data/train/a.csv" && obj.Key != "data/train/b.csv" {
			t.Errorf("unexpected key %q in prefix listing", obj.Key)
		}
	}
}

func TestPut_NestedKeyPaths(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	data := []byte("deeply nested content")
	key := "a/b/c/d/deep.txt"
	if err := s.Put(ctx, "bucket", key, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put nested key: %v", err)
	}

	// Verify it can be read back.
	rc, err := s.Get(ctx, "bucket", key)
	if err != nil {
		t.Fatalf("Get nested key: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("nested key round trip mismatch: got %q, want %q", got, data)
	}
}

func TestGet_NonExistentKey(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.Get(ctx, "bucket", "does-not-exist.txt")
	if err == nil {
		t.Fatal("expected error for non-existent key, got nil")
	}
}
