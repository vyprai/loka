// Package lock provides a distributed file lock manager for LOKA volumes.
// Workers acquire locks via the control plane API before writing to shared volumes.
// Locks have TTLs to prevent deadlocks from crashed workers.
// Locks are persisted to the database so they survive CP restarts.
package lock

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)


// Manager provides distributed file locking for volumes.
// Runs in the control plane. Workers acquire/release locks via HTTP API.
// In-memory map provides fast access; DB provides durability across restarts.
type Manager struct {
	mu    sync.Mutex
	locks map[string]*FileLock // key: "volume:path"
	db    *sql.DB              // Persistent store (nil = in-memory only).

	// Background reaper for expired locks.
	done chan struct{}
	wg   sync.WaitGroup
}

// FileLock represents an active file lock.
type FileLock struct {
	Volume     string    `json:"volume"`
	Path       string    `json:"path"`
	WorkerID   string    `json:"worker_id"`
	Exclusive  bool      `json:"exclusive"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// NewManager creates a new lock manager and starts the TTL reaper.
// If db is non-nil, locks are persisted to the file_locks table and
// restored on startup.
func NewManager(db *sql.DB) *Manager {
	m := &Manager{
		locks: make(map[string]*FileLock),
		db:    db,
		done:  make(chan struct{}),
	}

	// Restore locks from DB.
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
// Returns error if the file is already locked by another worker (exclusive).
func (m *Manager) Acquire(volume, path, workerID string, exclusive bool, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = 30 * time.Second // Default TTL.
	}
	if ttl > 10*time.Minute {
		ttl = 10 * time.Minute // Max TTL.
	}

	key := lockKey(volume, path)
	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.locks[key]; ok {
		// Check if expired.
		if now.After(existing.ExpiresAt) {
			delete(m.locks, key)
			m.deleteFromDB(key)
		} else if existing.WorkerID != workerID {
			return fmt.Errorf("file %s in volume %s is locked by worker %s (expires %s)",
				path, volume, existing.WorkerID, existing.ExpiresAt.Format(time.RFC3339))
		} else {
			// Same worker re-acquiring — extend TTL.
			existing.ExpiresAt = now.Add(ttl)
			m.upsertToDB(key, existing)
			return nil
		}
	}

	lock := &FileLock{
		Volume:     volume,
		Path:       path,
		WorkerID:   workerID,
		Exclusive:  exclusive,
		AcquiredAt: now,
		ExpiresAt:  now.Add(ttl),
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
	if lock.WorkerID != workerID {
		return fmt.Errorf("lock on %s:%s is held by worker %s, not %s",
			volume, path, lock.WorkerID, workerID)
	}

	delete(m.locks, key)
	m.deleteFromDB(key)
	return nil
}

// ReleaseAll releases all locks held by a worker (e.g., on disconnect).
func (m *Manager) ReleaseAll(workerID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	count := 0
	for key, lock := range m.locks {
		if lock.WorkerID == workerID {
			delete(m.locks, key)
			m.deleteFromDB(key)
			count++
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
	if time.Now().After(lock.ExpiresAt) {
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

	var result []*FileLock
	now := time.Now()
	for key, lock := range m.locks {
		if now.After(lock.ExpiresAt) {
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
				if now.After(lock.ExpiresAt) {
					delete(m.locks, key)
				}
			}
			m.mu.Unlock()

			// Bulk delete expired from DB.
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
		return // Table may not exist yet (pre-migration).
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
			continue // Skip expired.
		}
		m.locks[key] = &FileLock{
			Volume:     volume,
			Path:       path,
			WorkerID:   workerID,
			Exclusive:  exclusive == 1,
			AcquiredAt: acquired,
			ExpiresAt:  expires,
		}
	}
}

// upsertToDB persists a lock. Must be called with m.mu held.
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

// deleteFromDB removes a lock. Must be called with m.mu held.
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
