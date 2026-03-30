package lock

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS file_locks (
		lock_key TEXT PRIMARY KEY, volume TEXT NOT NULL, path TEXT NOT NULL,
		worker_id TEXT NOT NULL, exclusive INTEGER NOT NULL DEFAULT 1,
		acquired_at TEXT NOT NULL, expires_at TEXT NOT NULL
	)`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestAcquireRelease(t *testing.T) {
	db := setupTestDB(t)
	m := NewManager(db)
	defer m.Stop()

	err := m.Acquire("vol1", "/data/file.txt", "worker-1", true, 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	locked, lock := m.IsLocked("vol1", "/data/file.txt")
	if !locked {
		t.Fatal("expected file to be locked after Acquire")
	}
	if lock.WorkerID != "worker-1" {
		t.Errorf("WorkerID = %q, want %q", lock.WorkerID, "worker-1")
	}

	err = m.Release("vol1", "/data/file.txt", "worker-1")
	if err != nil {
		t.Fatalf("Release: %v", err)
	}

	locked, _ = m.IsLocked("vol1", "/data/file.txt")
	if locked {
		t.Error("expected file to be unlocked after Release")
	}
}

func TestAcquireConflict(t *testing.T) {
	db := setupTestDB(t)
	m := NewManager(db)
	defer m.Stop()

	err := m.Acquire("vol1", "/shared.txt", "worker-1", true, 5*time.Second)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	err = m.Acquire("vol1", "/shared.txt", "worker-2", true, 5*time.Second)
	if err == nil {
		t.Fatal("expected error when a different worker tries to acquire a held lock")
	}
}

func TestAcquireSameWorkerExtendsTTL(t *testing.T) {
	db := setupTestDB(t)
	m := NewManager(db)
	defer m.Stop()

	err := m.Acquire("vol1", "/f.txt", "worker-1", true, 2*time.Second)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	_, lock1 := m.IsLocked("vol1", "/f.txt")
	expires1 := lock1.ExpiresAt

	time.Sleep(50 * time.Millisecond)

	err = m.Acquire("vol1", "/f.txt", "worker-1", true, 5*time.Second)
	if err != nil {
		t.Fatalf("re-Acquire by same worker: %v", err)
	}

	_, lock2 := m.IsLocked("vol1", "/f.txt")
	if !lock2.ExpiresAt.After(expires1) {
		t.Errorf("ExpiresAt was not extended: before=%v after=%v", expires1, lock2.ExpiresAt)
	}
}

func TestReleaseWrongWorker(t *testing.T) {
	db := setupTestDB(t)
	m := NewManager(db)
	defer m.Stop()

	err := m.Acquire("vol1", "/f.txt", "worker-1", true, 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	err = m.Release("vol1", "/f.txt", "worker-2")
	if err == nil {
		t.Fatal("expected error when releasing a lock held by another worker")
	}

	// Original lock should still be held.
	locked, lock := m.IsLocked("vol1", "/f.txt")
	if !locked {
		t.Fatal("lock should still be held after wrong-worker release attempt")
	}
	if lock.WorkerID != "worker-1" {
		t.Errorf("WorkerID = %q, want %q", lock.WorkerID, "worker-1")
	}
}

func TestReleaseAll(t *testing.T) {
	db := setupTestDB(t)
	m := NewManager(db)
	defer m.Stop()

	// Worker-1 acquires several locks.
	for _, path := range []string{"/a.txt", "/b.txt", "/c.txt"} {
		if err := m.Acquire("vol1", path, "worker-1", true, 5*time.Second); err != nil {
			t.Fatalf("Acquire %s: %v", path, err)
		}
	}
	// Worker-2 acquires one lock.
	if err := m.Acquire("vol1", "/d.txt", "worker-2", true, 5*time.Second); err != nil {
		t.Fatalf("Acquire /d.txt: %v", err)
	}

	count := m.ReleaseAll("worker-1")
	if count != 3 {
		t.Errorf("ReleaseAll count = %d, want 3", count)
	}

	// Worker-1 locks gone.
	for _, path := range []string{"/a.txt", "/b.txt", "/c.txt"} {
		if locked, _ := m.IsLocked("vol1", path); locked {
			t.Errorf("lock for %s still held after ReleaseAll", path)
		}
	}

	// Worker-2 lock unaffected.
	if locked, _ := m.IsLocked("vol1", "/d.txt"); !locked {
		t.Error("worker-2 lock should not have been released")
	}
}

func TestTTLExpiry(t *testing.T) {
	db := setupTestDB(t)
	m := NewManager(db)
	defer m.Stop()

	err := m.Acquire("vol1", "/tmp.txt", "worker-1", true, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	locked, _ := m.IsLocked("vol1", "/tmp.txt")
	if !locked {
		t.Fatal("expected lock to be held immediately after Acquire")
	}

	time.Sleep(150 * time.Millisecond)

	locked, _ = m.IsLocked("vol1", "/tmp.txt")
	if locked {
		t.Error("expected lock to be expired after TTL")
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	db := setupTestDB(t)

	// First manager acquires a lock.
	m1 := NewManager(db)
	err := m1.Acquire("vol1", "/persist.txt", "worker-1", true, 30*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	m1.Stop()

	// Second manager with the same DB should see the lock.
	m2 := NewManager(db)
	defer m2.Stop()

	locked, lock := m2.IsLocked("vol1", "/persist.txt")
	if !locked {
		t.Fatal("lock should survive manager restart via DB persistence")
	}
	if lock.WorkerID != "worker-1" {
		t.Errorf("WorkerID = %q, want %q", lock.WorkerID, "worker-1")
	}
}

func TestNilDB(t *testing.T) {
	m := NewManager(nil)
	defer m.Stop()

	err := m.Acquire("vol1", "/mem.txt", "worker-1", true, 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire with nil DB: %v", err)
	}

	locked, _ := m.IsLocked("vol1", "/mem.txt")
	if !locked {
		t.Error("expected lock to be held in-memory mode")
	}

	err = m.Release("vol1", "/mem.txt", "worker-1")
	if err != nil {
		t.Fatalf("Release with nil DB: %v", err)
	}

	locked, _ = m.IsLocked("vol1", "/mem.txt")
	if locked {
		t.Error("expected lock to be released in-memory mode")
	}
}
