// Package lock provides a distributed file lock manager for LOKA volumes.
// Workers acquire locks via the control plane API before writing to shared volumes.
// Locks have TTLs to prevent deadlocks from crashed workers.
// Locks are persisted to the database so they survive CP restarts.
//
// Supports shared (non-exclusive) locks: multiple workers can hold a shared lock
// on the same file simultaneously. An exclusive lock blocks all other holders.
package lock

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/vyprai/loka/internal/controlplane/metrics/recorder"
	"github.com/vyprai/loka/internal/metrics"
)

// Manager provides distributed file locking for volumes.
// Runs in the control plane. Workers acquire/release locks via HTTP API.
// In-memory map provides fast access; DB provides durability across restarts.
type Manager struct {
	mu       sync.Mutex
	locks    map[string]*FileLock // key: "volume:path"
	db       *sql.DB              // Persistent store (nil = in-memory only).
	recorder recorder.Recorder

	// Background reaper for expired locks.
	done chan struct{}
	wg   sync.WaitGroup
}

// LockHolder represents a single holder of a lock.
type LockHolder struct {
	WorkerID  string    `json:"worker_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// FileLock represents an active file lock with support for multiple shared holders.
type FileLock struct {
	Volume     string       `json:"volume"`
	Path       string       `json:"path"`
	Holders    []LockHolder `json:"holders"`
	Exclusive  bool         `json:"exclusive"`
	AcquiredAt time.Time    `json:"acquired_at"`
	// WorkerID is the primary holder (first holder for shared, sole holder for exclusive).
	// Kept for backward compatibility with API responses.
	WorkerID  string    `json:"worker_id"`
	ExpiresAt time.Time `json:"expires_at"` // Latest expiry across all holders.
}

// NewManager creates a new lock manager and starts the TTL reaper.
func NewManager(db *sql.DB, rec recorder.Recorder) *Manager {
	if rec == nil {
		rec = recorder.NopRecorder{}
	}
	m := &Manager{
		locks:    make(map[string]*FileLock),
		db:       db,
		recorder: rec,
		done:     make(chan struct{}),
	}

	if db != nil {
		m.loadFromDB()
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.reapExpired()
	}()
	return m
}

// Stop shuts down the lock manager and waits for the reaper to finish.
func (m *Manager) Stop() {
	close(m.done)
	m.wg.Wait()
}

// Acquire attempts to acquire a lock on a file in a volume.
// Shared locks: multiple workers can hold the same lock concurrently.
// Exclusive locks: only one worker can hold the lock.
func (m *Manager) Acquire(volume, path, workerID string, exclusive bool, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if ttl > 10*time.Minute {
		ttl = 10 * time.Minute
	}

	key := lockKey(volume, path)
	now := time.Now()
	expiresAt := now.Add(ttl)

	m.mu.Lock()
	defer m.mu.Unlock()

	existing, ok := m.locks[key]
	if ok {
		// Purge expired holders.
		existing.Holders = purgeExpiredHolders(existing.Holders, now)
		if len(existing.Holders) == 0 {
			// All holders expired — reclaim the lock.
			delete(m.locks, key)
			m.deleteFromDB(key)
			ok = false
		}
	}

	if ok {
		// Check for conflicts.
		if exclusive {
			// Exclusive request: fails if any holder exists (unless same worker re-acquiring).
			if len(existing.Holders) == 1 && existing.Holders[0].WorkerID == workerID {
				// Same worker re-acquiring — extend TTL and upgrade to exclusive.
				existing.Holders[0].ExpiresAt = expiresAt
				existing.Exclusive = true
				existing.WorkerID = workerID
				existing.ExpiresAt = expiresAt
				m.upsertToDB(key, existing)
				return nil
			}
			m.recorder.Inc("lock_contention_total", metrics.Label{Name: "volume", Value: volume}, metrics.Label{Name: "path", Value: path}, metrics.Label{Name: "type", Value: "exclusive_denied"})
			return fmt.Errorf("file %s in volume %s is locked by %d holder(s) — cannot acquire exclusive",
				path, volume, len(existing.Holders))
		}

		// Shared request.
		if existing.Exclusive {
			// Existing lock is exclusive — deny shared.
			if existing.Holders[0].WorkerID == workerID {
				// Same worker: extend TTL.
				existing.Holders[0].ExpiresAt = expiresAt
				existing.ExpiresAt = expiresAt
				m.upsertToDB(key, existing)
				return nil
			}
			m.recorder.Inc("lock_contention_total", metrics.Label{Name: "volume", Value: volume}, metrics.Label{Name: "path", Value: path}, metrics.Label{Name: "type", Value: "shared_denied"})
			return fmt.Errorf("file %s in volume %s is exclusively locked by worker %s",
				path, volume, existing.WorkerID)
		}

		// Existing lock is shared — add this worker as another holder.
		for i, h := range existing.Holders {
			if h.WorkerID == workerID {
				// Same worker re-acquiring — extend TTL.
				existing.Holders[i].ExpiresAt = expiresAt
				existing.ExpiresAt = maxExpiry(existing.Holders)
				m.upsertToDB(key, existing)
				return nil
			}
		}
		existing.Holders = append(existing.Holders, LockHolder{
			WorkerID:  workerID,
			ExpiresAt: expiresAt,
		})
		existing.ExpiresAt = maxExpiry(existing.Holders)
		m.upsertToDB(key, existing)
		return nil
	}

	m.recorder.Inc("lock_acquisitions_total", metrics.Label{Name: "volume", Value: volume}, metrics.Label{Name: "path", Value: path}, metrics.Label{Name: "worker_id", Value: workerID})

	// No existing lock — create new.
	lock := &FileLock{
		Volume: volume,
		Path:   path,
		Holders: []LockHolder{{
			WorkerID:  workerID,
			ExpiresAt: expiresAt,
		}},
		Exclusive:  exclusive,
		WorkerID:   workerID,
		AcquiredAt: now,
		ExpiresAt:  expiresAt,
	}
	m.locks[key] = lock
	m.upsertToDB(key, lock)
	return nil
}

// Release releases a lock held by a worker.
func (m *Manager) Release(volume, path, workerID string) error {
	key := lockKey(volume, path)

	m.mu.Lock()
	defer m.mu.Unlock()

	lock, ok := m.locks[key]
	if !ok {
		return nil // Already released.
	}

	// Remove the worker from holders.
	var remaining []LockHolder
	found := false
	for _, h := range lock.Holders {
		if h.WorkerID == workerID {
			found = true
			continue
		}
		remaining = append(remaining, h)
	}
	if !found {
		return fmt.Errorf("lock on %s:%s is not held by worker %s", volume, path, workerID)
	}

	if len(remaining) == 0 {
		delete(m.locks, key)
		m.deleteFromDB(key)
	} else {
		lock.Holders = remaining
		lock.WorkerID = remaining[0].WorkerID
		lock.ExpiresAt = maxExpiry(remaining)
		m.upsertToDB(key, lock)
	}
	return nil
}

// ReleaseAll releases all locks held by a worker (e.g., on disconnect).
func (m *Manager) ReleaseAll(workerID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	count := 0
	for key, lock := range m.locks {
		var remaining []LockHolder
		released := false
		for _, h := range lock.Holders {
			if h.WorkerID == workerID {
				released = true
				continue
			}
			remaining = append(remaining, h)
		}
		if !released {
			continue
		}
		count++
		if len(remaining) == 0 {
			delete(m.locks, key)
			m.deleteFromDB(key)
		} else {
			lock.Holders = remaining
			lock.WorkerID = remaining[0].WorkerID
			lock.ExpiresAt = maxExpiry(remaining)
			m.upsertToDB(key, lock)
		}
	}
	return count
}

// IsLocked checks if a file is locked.
func (m *Manager) IsLocked(volume, path string) (bool, *FileLock) {
	key := lockKey(volume, path)

	m.mu.Lock()
	defer m.mu.Unlock()

	lock, ok := m.locks[key]
	if !ok {
		return false, nil
	}
	lock.Holders = purgeExpiredHolders(lock.Holders, time.Now())
	if len(lock.Holders) == 0 {
		delete(m.locks, key)
		m.deleteFromDB(key)
		return false, nil
	}
	return true, lock
}

// ListLocks returns all active locks for a volume.
func (m *Manager) ListLocks(volume string) []*FileLock {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	var result []*FileLock
	for key, lock := range m.locks {
		lock.Holders = purgeExpiredHolders(lock.Holders, now)
		if len(lock.Holders) == 0 {
			delete(m.locks, key)
			m.deleteFromDB(key)
			continue
		}
		if lock.Volume == volume {
			result = append(result, lock)
		}
	}
	return result
}

// reapExpired periodically removes expired locks from memory and DB.
func (m *Manager) reapExpired() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			m.mu.Lock()
			now := time.Now()
			for key, lock := range m.locks {
				lock.Holders = purgeExpiredHolders(lock.Holders, now)
				if len(lock.Holders) == 0 {
					delete(m.locks, key)
				}
			}
			m.mu.Unlock()

			if m.db != nil {
				m.db.ExecContext(context.Background(),
					`DELETE FROM file_locks WHERE expires_at < ?`,
					time.Now().UTC().Format(time.RFC3339))
			}
		}
	}
}

// ── DB persistence (best-effort, errors logged but not fatal) ──

func (m *Manager) loadFromDB() {
	rows, err := m.db.QueryContext(context.Background(),
		`SELECT lock_key, volume, path, worker_id, exclusive, acquired_at, expires_at FROM file_locks`)
	if err != nil {
		return
	}
	defer rows.Close()

	now := time.Now()
	for rows.Next() {
		var key, volume, path, workerID, acquiredStr, expiresStr string
		var exclusive int
		if err := rows.Scan(&key, &volume, &path, &workerID, &exclusive, &acquiredStr, &expiresStr); err != nil {
			continue
		}
		acquired, _ := time.Parse(time.RFC3339, acquiredStr)
		expires, _ := time.Parse(time.RFC3339, expiresStr)
		if now.After(expires) {
			continue
		}
		m.locks[key] = &FileLock{
			Volume: volume,
			Path:   path,
			Holders: []LockHolder{{
				WorkerID:  workerID,
				ExpiresAt: expires,
			}},
			Exclusive:  exclusive == 1,
			WorkerID:   workerID,
			AcquiredAt: acquired,
			ExpiresAt:  expires,
		}
	}
}

func (m *Manager) upsertToDB(key string, lock *FileLock) {
	if m.db == nil {
		return
	}
	exclusive := 0
	if lock.Exclusive {
		exclusive = 1
	}
	m.db.ExecContext(context.Background(),
		`INSERT INTO file_locks (lock_key, volume, path, worker_id, exclusive, acquired_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(lock_key) DO UPDATE SET
		   worker_id = excluded.worker_id,
		   exclusive = excluded.exclusive,
		   acquired_at = excluded.acquired_at,
		   expires_at = excluded.expires_at`,
		key, lock.Volume, lock.Path, lock.WorkerID, exclusive,
		lock.AcquiredAt.UTC().Format(time.RFC3339),
		lock.ExpiresAt.UTC().Format(time.RFC3339))
}

func (m *Manager) deleteFromDB(key string) {
	if m.db == nil {
		return
	}
	m.db.ExecContext(context.Background(),
		`DELETE FROM file_locks WHERE lock_key = ?`, key)
}

func lockKey(volume, path string) string {
	return volume + ":" + path
}

func purgeExpiredHolders(holders []LockHolder, now time.Time) []LockHolder {
	var active []LockHolder
	for _, h := range holders {
		if now.Before(h.ExpiresAt) {
			active = append(active, h)
		}
	}
	return active
}

func maxExpiry(holders []LockHolder) time.Time {
	var max time.Time
	for _, h := range holders {
		if h.ExpiresAt.After(max) {
			max = h.ExpiresAt
		}
	}
	return max
}
