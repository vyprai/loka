package tsdb

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/metrics"
)

func testDataPoints(n int) []metrics.DataPoint {
	points := make([]metrics.DataPoint, n)
	for i := range points {
		points[i] = metrics.DataPoint{
			Name:      "test_metric",
			Type:      metrics.Gauge,
			Labels:    metrics.Labels{{Name: "host", Value: "node-1"}},
			Timestamp: time.Now().UnixMilli() + int64(i),
			Value:     float64(i) * 1.5,
		}
	}
	return points
}

func TestWALWriteReadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWAL(WALConfig{Dir: dir}, nil)
	if err != nil {
		t.Fatalf("NewWAL: %v", err)
	}

	written := testDataPoints(5)
	if err := w.Append(written); err != nil {
		t.Fatalf("Append: %v", err)
	}
	seqNo := w.LatestSeqNo()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reader := NewWALReader(dir)
	got, err := reader.ReadSegment(seqNo)
	if err != nil {
		t.Fatalf("ReadSegment: %v", err)
	}

	if len(got) != len(written) {
		t.Fatalf("got %d points, want %d", len(got), len(written))
	}
	for i, dp := range got {
		if dp.Name != written[i].Name {
			t.Errorf("point[%d].Name = %q, want %q", i, dp.Name, written[i].Name)
		}
		if dp.Value != written[i].Value {
			t.Errorf("point[%d].Value = %f, want %f", i, dp.Value, written[i].Value)
		}
		if dp.Timestamp != written[i].Timestamp {
			t.Errorf("point[%d].Timestamp = %d, want %d", i, dp.Timestamp, written[i].Timestamp)
		}
	}
}

func TestWALSegmentRotation(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWAL(WALConfig{Dir: dir, SegmentMaxSize: 100}, nil)
	if err != nil {
		t.Fatalf("NewWAL: %v", err)
	}

	// Write enough data to force multiple rotations.
	for i := 0; i < 20; i++ {
		if err := w.Append(testDataPoints(3)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reader := NewWALReader(dir)
	segs, err := reader.ListSegments()
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	if len(segs) < 2 {
		t.Fatalf("expected multiple segments, got %d", len(segs))
	}
	t.Logf("created %d segments", len(segs))
}

func TestWALCleanBefore(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWAL(WALConfig{Dir: dir, SegmentMaxSize: 100}, nil)
	if err != nil {
		t.Fatalf("NewWAL: %v", err)
	}

	for i := 0; i < 20; i++ {
		if err := w.Append(testDataPoints(3)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	reader := NewWALReader(dir)
	segsBefore, err := reader.ListSegments()
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	if len(segsBefore) < 3 {
		t.Fatalf("need at least 3 segments for test, got %d", len(segsBefore))
	}

	// Clean segments before the third one.
	cutoff := segsBefore[2]
	if err := w.CleanBefore(cutoff); err != nil {
		t.Fatalf("CleanBefore: %v", err)
	}

	segsAfter, err := reader.ListSegments()
	if err != nil {
		t.Fatalf("ListSegments after clean: %v", err)
	}
	for _, sn := range segsAfter {
		if sn < cutoff {
			t.Errorf("segment %d should have been cleaned (cutoff=%d)", sn, cutoff)
		}
	}
	if len(segsAfter) >= len(segsBefore) {
		t.Errorf("expected fewer segments after clean: before=%d, after=%d", len(segsBefore), len(segsAfter))
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestWALSegmentsAfter(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWAL(WALConfig{Dir: dir, SegmentMaxSize: 100}, nil)
	if err != nil {
		t.Fatalf("NewWAL: %v", err)
	}

	for i := 0; i < 20; i++ {
		if err := w.Append(testDataPoints(3)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reader := NewWALReader(dir)
	allSegs, err := reader.ListSegments()
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	if len(allSegs) < 3 {
		t.Fatalf("need at least 3 segments, got %d", len(allSegs))
	}

	pivot := allSegs[1]
	after, err := reader.SegmentsAfter(pivot)
	if err != nil {
		t.Fatalf("SegmentsAfter: %v", err)
	}
	for _, sn := range after {
		if sn <= pivot {
			t.Errorf("SegmentsAfter(%d) returned %d", pivot, sn)
		}
	}
	// All segments after the pivot should be returned.
	expected := 0
	for _, sn := range allSegs {
		if sn > pivot {
			expected++
		}
	}
	if len(after) != expected {
		t.Errorf("got %d segments after %d, want %d", len(after), pivot, expected)
	}
}

func TestWALEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWAL(WALConfig{Dir: dir}, nil)
	if err != nil {
		t.Fatalf("NewWAL: %v", err)
	}

	if seq := w.LatestSeqNo(); seq != 1 {
		t.Errorf("expected seqNo=1 on empty dir, got %d", seq)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestWALResumeExisting(t *testing.T) {
	dir := t.TempDir()

	// First WAL instance.
	w1, err := NewWAL(WALConfig{Dir: dir, SegmentMaxSize: 100}, nil)
	if err != nil {
		t.Fatalf("NewWAL(1): %v", err)
	}
	for i := 0; i < 10; i++ {
		if err := w1.Append(testDataPoints(3)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	seqBefore := w1.LatestSeqNo()
	if err := w1.Close(); err != nil {
		t.Fatalf("Close(1): %v", err)
	}

	// Second WAL instance on same directory.
	w2, err := NewWAL(WALConfig{Dir: dir, SegmentMaxSize: 100}, nil)
	if err != nil {
		t.Fatalf("NewWAL(2): %v", err)
	}
	seqAfter := w2.LatestSeqNo()
	if seqAfter <= seqBefore {
		t.Errorf("seqNo did not advance: before=%d, after=%d", seqBefore, seqAfter)
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("Close(2): %v", err)
	}

	// Verify all old segments are still readable.
	reader := NewWALReader(dir)
	segs, err := reader.ListSegments()
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	if len(segs) == 0 {
		t.Fatal("expected segments, got none")
	}
}

func TestWALConcurrentAppend(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWAL(WALConfig{Dir: dir}, nil)
	if err != nil {
		t.Fatalf("NewWAL: %v", err)
	}

	var wg sync.WaitGroup
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if err := w.Append(testDataPoints(2)); err != nil {
					t.Errorf("concurrent Append: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify data can be read back from all segments.
	reader := NewWALReader(dir)
	segs, err := reader.ListSegments()
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}

	totalPoints := 0
	for _, sn := range segs {
		points, err := reader.ReadSegment(sn)
		if err != nil {
			t.Fatalf("ReadSegment(%d): %v", sn, err)
		}
		totalPoints += len(points)
	}

	expected := 10 * 50 * 2 // 10 goroutines * 50 iterations * 2 points
	if totalPoints != expected {
		t.Errorf("total points = %d, want %d", totalPoints, expected)
	}

	// Also verify no stray non-WAL files.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		matched, _ := filepath.Match("wal-*.log", e.Name())
		if !matched {
			t.Errorf("unexpected file in WAL dir: %s", e.Name())
		}
	}
}
