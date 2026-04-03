package lock

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// ═══════════════════════════════════════════════════════════════
// Edge Cases: TTL Boundaries
// ═══════════════════════════════════════════════════════════════

func TestTTL_DefaultApplied(t *testing.T) {
	m := NewManager(nil, nil)
	defer m.Stop()

	m.Acquire("vol", "/f", "w1", true, 0) // TTL=0 → default 30s
	_, lock := m.IsLocked("vol", "/f")
	if lock == nil {
		t.Fatal("expected lock")
	}
	remaining := time.Until(lock.ExpiresAt)
	if remaining < 25*time.Second || remaining > 31*time.Second {
		t.Errorf("expected ~30s TTL, got %v", remaining)
	}
}

func TestTTL_MaxClamped(t *testing.T) {
	m := NewManager(nil, nil)
	defer m.Stop()

	m.Acquire("vol", "/f", "w1", true, 1*time.Hour) // > 10min → clamped
	_, lock := m.IsLocked("vol", "/f")
	remaining := time.Until(lock.ExpiresAt)
	if remaining > 10*time.Minute+time.Second {
		t.Errorf("expected ≤10min TTL, got %v", remaining)
	}
}

func TestTTL_ExactExpiry(t *testing.T) {
	m := NewManager(nil, nil)
	defer m.Stop()

	m.Acquire("vol", "/f", "w1", true, 1*time.Millisecond)
	time.Sleep(5 * time.Millisecond) // wait for expiry

	locked, _ := m.IsLocked("vol", "/f")
	if locked {
		t.Error("lock should have expired")
	}

	// Another worker should be able to acquire.
	if err := m.Acquire("vol", "/f", "w2", true, 5*time.Second); err != nil {
		t.Fatalf("acquire after expiry: %v", err)
	}
}

// ═══════════════════════════════════════════════════════════════
// Edge Cases: Shared Lock Upgrade/Downgrade
// ═══════════════════════════════════════════════════════════════

func TestSharedToExclusive_SameWorker(t *testing.T) {
	m := NewManager(nil, nil)
	defer m.Stop()

	m.Acquire("vol", "/f", "w1", false, 5*time.Second)

	// Same worker upgrades to exclusive (only holder).
	if err := m.Acquire("vol", "/f", "w1", true, 5*time.Second); err != nil {
		t.Fatalf("upgrade to exclusive: %v", err)
	}

	_, lock := m.IsLocked("vol", "/f")
	if !lock.Exclusive {
		t.Error("expected exclusive after upgrade")
	}
}

func TestSharedToExclusive_FailsWithMultipleHolders(t *testing.T) {
	m := NewManager(nil, nil)
	defer m.Stop()

	m.Acquire("vol", "/f", "w1", false, 5*time.Second)
	m.Acquire("vol", "/f", "w2", false, 5*time.Second)

	// w1 tries to upgrade — should fail, w2 is also holding.
	if err := m.Acquire("vol", "/f", "w1", true, 5*time.Second); err == nil {
		t.Fatal("exclusive upgrade should fail with 2 holders")
	}
}

func TestExclusiveToShared_SameWorker(t *testing.T) {
	m := NewManager(nil, nil)
	defer m.Stop()

	m.Acquire("vol", "/f", "w1", true, 5*time.Second)

	// Same worker re-acquires as shared — extends TTL (downgrade).
	if err := m.Acquire("vol", "/f", "w1", false, 5*time.Second); err != nil {
		t.Fatalf("downgrade to shared: %v", err)
	}
}

// ═══════════════════════════════════════════════════════════════
// Edge Cases: Expired Holder Cleanup
// ═══════════════════════════════════════════════════════════════

func TestExpiredHolder_ReclaimsLock(t *testing.T) {
	m := NewManager(nil, nil)
	defer m.Stop()

	m.Acquire("vol", "/f", "w1", true, 1*time.Millisecond)
	time.Sleep(5 * time.Millisecond)

	// w2 should acquire after expiry.
	if err := m.Acquire("vol", "/f", "w2", true, 5*time.Second); err != nil {
		t.Fatalf("acquire after expiry: %v", err)
	}
	_, lock := m.IsLocked("vol", "/f")
	if lock.WorkerID != "w2" {
		t.Errorf("expected w2, got %s", lock.WorkerID)
	}
}

func TestExpiredSharedHolder_RemovedOnAcquire(t *testing.T) {
	m := NewManager(nil, nil)
	defer m.Stop()

	m.Acquire("vol", "/f", "w1", false, 1*time.Millisecond)
	m.Acquire("vol", "/f", "w2", false, 5*time.Second)

	time.Sleep(5 * time.Millisecond) // w1 expires

	// w3 acquires exclusive — should succeed because w1 expired, only w2 remains shared.
	// w2 is still shared → exclusive should fail.
	err := m.Acquire("vol", "/f", "w3", true, 5*time.Second)
	if err == nil {
		t.Fatal("exclusive should fail, w2 still holds shared lock")
	}
}

// ═══════════════════════════════════════════════════════════════
// Edge Cases: Release Edge Cases
// ═══════════════════════════════════════════════════════════════

func TestRelease_NotHolder(t *testing.T) {
	m := NewManager(nil, nil)
	defer m.Stop()

	m.Acquire("vol", "/f", "w1", true, 5*time.Second)
	err := m.Release("vol", "/f", "w2")
	if err == nil {
		t.Fatal("should not release lock held by another worker")
	}
}

func TestRelease_AlreadyReleased(t *testing.T) {
	m := NewManager(nil, nil)
	defer m.Stop()

	// Release on non-existent lock → should not error.
	if err := m.Release("vol", "/f", "w1"); err != nil {
		t.Fatalf("release non-existent should be idempotent: %v", err)
	}
}

func TestRelease_SharedLockKeepsOtherHolders(t *testing.T) {
	m := NewManager(nil, nil)
	defer m.Stop()

	m.Acquire("vol", "/f", "w1", false, 5*time.Second)
	m.Acquire("vol", "/f", "w2", false, 5*time.Second)
	m.Acquire("vol", "/f", "w3", false, 5*time.Second)

	m.Release("vol", "/f", "w2")

	_, lock := m.IsLocked("vol", "/f")
	if len(lock.Holders) != 2 {
		t.Fatalf("expected 2 holders, got %d", len(lock.Holders))
	}
	for _, h := range lock.Holders {
		if h.WorkerID == "w2" {
			t.Error("w2 should have been removed")
		}
	}
}

func TestReleaseAll_MixedLockTypes(t *testing.T) {
	m := NewManager(nil, nil)
	defer m.Stop()

	m.Acquire("vol", "/a", "w1", true, 5*time.Second)       // exclusive
	m.Acquire("vol", "/b", "w1", false, 5*time.Second)       // shared
	m.Acquire("vol", "/b", "w2", false, 5*time.Second)       // shared by w2 too
	m.Acquire("vol", "/c", "w2", true, 5*time.Second)         // exclusive by w2

	count := m.ReleaseAll("w1")
	if count != 2 {
		t.Errorf("expected 2 releases, got %d", count)
	}

	// /a: gone
	locked, _ := m.IsLocked("vol", "/a")
	if locked {
		t.Error("/a should be unlocked")
	}

	// /b: w2 still holds
	locked, lock := m.IsLocked("vol", "/b")
	if !locked || len(lock.Holders) != 1 || lock.Holders[0].WorkerID != "w2" {
		t.Error("/b should be held only by w2")
	}

	// /c: untouched
	locked, _ = m.IsLocked("vol", "/c")
	if !locked {
		t.Error("/c should still be locked by w2")
	}
}

// ═══════════════════════════════════════════════════════════════
// Edge Cases: Concurrent Lock Operations
// ═══════════════════════════════════════════════════════════════

func TestConcurrentSharedAcquire(t *testing.T) {
	m := NewManager(nil, nil)
	defer m.Stop()

	var wg sync.WaitGroup
	errors := make([]error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			workerID := fmt.Sprintf("w%d", idx)
			errors[idx] = m.Acquire("vol", "/shared-file", workerID, false, 5*time.Second)
		}(i)
	}
	wg.Wait()

	// All shared acquires should succeed.
	for i, err := range errors {
		if err != nil {
			t.Errorf("worker w%d shared acquire failed: %v", i, err)
		}
	}

	_, lock := m.IsLocked("vol", "/shared-file")
	if len(lock.Holders) != 20 {
		t.Errorf("expected 20 holders, got %d", len(lock.Holders))
	}
}

func TestConcurrentExclusiveAcquire(t *testing.T) {
	m := NewManager(nil, nil)
	defer m.Stop()

	var wg sync.WaitGroup
	successes := 0
	var mu sync.Mutex

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			workerID := fmt.Sprintf("w%d", idx)
			if err := m.Acquire("vol", "/exclusive-file", workerID, true, 5*time.Second); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	// Exactly 1 should succeed.
	if successes != 1 {
		t.Errorf("expected exactly 1 exclusive acquire to succeed, got %d", successes)
	}
}

func TestConcurrentAcquireRelease(t *testing.T) {
	m := NewManager(nil, nil)
	defer m.Stop()

	// Stress test: rapidly acquire and release from many workers.
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			workerID := fmt.Sprintf("w%d", idx)
			m.Acquire("vol", "/stress", workerID, false, 5*time.Second)
			time.Sleep(time.Millisecond)
			m.Release("vol", "/stress", workerID)
		}(i)
	}
	wg.Wait()

	// After all release, lock should be gone.
	locked, _ := m.IsLocked("vol", "/stress")
	if locked {
		t.Error("expected lock to be fully released after all workers release")
	}
}

// ═══════════════════════════════════════════════════════════════
// Edge Cases: DB Persistence with Shared Locks
// ═══════════════════════════════════════════════════════════════

func TestPersistence_SharedLock_RestoresFirstHolder(t *testing.T) {
	db := setupTestDB(t)

	// Manager 1: acquire shared lock.
	m1 := NewManager(db, nil)
	m1.Acquire("vol", "/f", "w1", false, 30*time.Second)
	m1.Acquire("vol", "/f", "w2", false, 30*time.Second)
	m1.Stop()

	// Manager 2: restart from same DB.
	m2 := NewManager(db, nil)
	defer m2.Stop()

	locked, lock := m2.IsLocked("vol", "/f")
	if !locked {
		t.Fatal("expected lock to persist across restart")
	}
	// DB only stores first holder's worker_id — this is a known limitation.
	if len(lock.Holders) < 1 {
		t.Error("expected at least 1 holder after restart")
	}
}

// ═══════════════════════════════════════════════════════════════
// Edge Cases: ListLocks
// ═══════════════════════════════════════════════════════════════

func TestListLocks_FiltersExpired(t *testing.T) {
	m := NewManager(nil, nil)
	defer m.Stop()

	m.Acquire("vol", "/a", "w1", true, 1*time.Millisecond)
	m.Acquire("vol", "/b", "w2", true, 5*time.Second)

	time.Sleep(5 * time.Millisecond)

	locks := m.ListLocks("vol")
	if len(locks) != 1 {
		t.Fatalf("expected 1 active lock, got %d", len(locks))
	}
	if locks[0].Path != "/b" {
		t.Errorf("expected /b, got %s", locks[0].Path)
	}
}

func TestListLocks_DifferentVolumes(t *testing.T) {
	m := NewManager(nil, nil)
	defer m.Stop()

	m.Acquire("vol1", "/a", "w1", true, 5*time.Second)
	m.Acquire("vol2", "/b", "w2", true, 5*time.Second)

	locks := m.ListLocks("vol1")
	if len(locks) != 1 {
		t.Fatalf("expected 1 lock for vol1, got %d", len(locks))
	}
}
