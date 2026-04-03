package collector

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/controlplane/metrics/tsdb"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store/sqlite"
)

func newTestTSDB(t *testing.T) tsdb.MetricsStore {
	t.Helper()
	s, err := tsdb.NewStore(tsdb.StoreConfig{DataDir: t.TempDir(), Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCollectorCollectsSessionCounts(t *testing.T) {
	db, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Migrate(context.Background())

	tsdbStore := newTestTSDB(t)

	// Create some sessions in different states.
	for i, status := range []loka.SessionStatus{loka.SessionStatusRunning, loka.SessionStatusRunning, loka.SessionStatusTerminated} {
		sess := &loka.Session{
			ID:     fmt.Sprintf("sess_%d", i),
			Name:   fmt.Sprintf("test-%d", i),
			Status: status,
		}
		db.Sessions().Create(context.Background(), sess)
	}

	c := New(db, tsdbStore, time.Second, nil)
	// Run collect manually (not via goroutine).
	c.collect(context.Background())

	// Query the TSDB for sessions_total metric.
	names, err := tsdbStore.ListMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, n := range names {
		if n == "sessions_total" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected sessions_total metric, got names: %v", names)
	}
}

func TestCollectorWritesSelfMonitoring(t *testing.T) {
	db, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Migrate(context.Background())

	tsdbStore := newTestTSDB(t)

	c := New(db, tsdbStore, time.Second, nil)
	c.collect(context.Background())

	names, _ := tsdbStore.ListMetrics(context.Background())
	nameSet := make(map[string]bool)
	for _, n := range names {
		nameSet[n] = true
	}

	if !nameSet["loka_tsdb_disk_bytes"] {
		t.Error("expected loka_tsdb_disk_bytes metric")
	}
	if !nameSet["loka_tsdb_write_samples_total"] {
		t.Error("expected loka_tsdb_write_samples_total metric")
	}
}

func TestCollectorStartStop(t *testing.T) {
	db, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Migrate(context.Background())

	tsdbStore := newTestTSDB(t)

	c := New(db, tsdbStore, 50*time.Millisecond, nil)
	c.Start()

	// Let it run a couple cycles.
	time.Sleep(150 * time.Millisecond)

	c.Stop()

	// Verify metrics were written.
	names, _ := tsdbStore.ListMetrics(context.Background())
	if len(names) == 0 {
		t.Error("expected some metrics after collector ran")
	}
}

func TestCollectorHandlesEmptyStore(t *testing.T) {
	db, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Migrate(context.Background())

	tsdbStore := newTestTSDB(t)

	c := New(db, tsdbStore, time.Second, nil)
	// Should not panic with empty database.
	c.collect(context.Background())

	// Self-monitoring should still be written.
	names, _ := tsdbStore.ListMetrics(context.Background())
	found := false
	for _, n := range names {
		if n == "loka_tsdb_disk_bytes" {
			found = true
		}
	}
	if !found {
		t.Error("expected self-monitoring metrics even with empty store")
	}
}
