package tsdb

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/vyprai/loka/internal/metrics"
)

// WALConfig configures the Write-Ahead Log for HA replication.
type WALConfig struct {
	Dir            string        // WAL segment directory.
	SegmentMaxSize int64         // Max bytes per segment (default 1MB).
	SyncInterval   time.Duration // How often to fsync (default 1s).
}

// WAL provides a write-ahead log for metrics data points.
// The leader writes data to WAL segments before committing to BadgerDB.
// Followers replay WAL segments to replicate data.
type WAL struct {
	mu             sync.Mutex
	dir            string
	segmentMaxSize int64
	currentFile    *os.File
	currentSize    int64
	currentSeqNo   int64
	logger         *slog.Logger
	cancel         context.CancelFunc
	wg             sync.WaitGroup
}

// NewWAL creates a new WAL writer.
func NewWAL(cfg WALConfig, logger *slog.Logger) (*WAL, error) {
	if cfg.SegmentMaxSize == 0 {
		cfg.SegmentMaxSize = 1 << 20 // 1MB
	}
	if cfg.SyncInterval == 0 {
		cfg.SyncInterval = time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("create WAL dir: %w", err)
	}

	// Find the latest sequence number from existing segments.
	seqNo := findLatestSeqNo(cfg.Dir)

	w := &WAL{
		dir:            cfg.Dir,
		segmentMaxSize: cfg.SegmentMaxSize,
		currentSeqNo:   seqNo,
		logger:         logger,
	}

	// Open a new segment.
	if err := w.rotate(); err != nil {
		return nil, err
	}

	// Start periodic fsync.
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.wg.Add(1)
	go w.syncLoop(ctx, cfg.SyncInterval)

	return w, nil
}

// Append writes data points to the WAL. Each entry is a length-prefixed JSON blob.
func (w *WAL) Append(points []metrics.DataPoint) error {
	if len(points) == 0 {
		return nil
	}

	data, err := json.Marshal(points)
	if err != nil {
		return fmt.Errorf("marshal WAL entry: %w", err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	// Rotate if current segment is too large.
	if w.currentSize+int64(len(data)+4) > w.segmentMaxSize {
		if err := w.rotate(); err != nil {
			return err
		}
	}

	// Write length prefix (4 bytes, big-endian) + data.
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	if _, err := w.currentFile.Write(lenBuf[:]); err != nil {
		return fmt.Errorf("write WAL length: %w", err)
	}
	if _, err := w.currentFile.Write(data); err != nil {
		return fmt.Errorf("write WAL data: %w", err)
	}

	w.currentSize += int64(len(data) + 4)
	return nil
}

// Close flushes and closes the WAL.
func (w *WAL) Close() error {
	w.cancel()
	w.wg.Wait()

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.currentFile != nil {
		w.currentFile.Sync()
		return w.currentFile.Close()
	}
	return nil
}

// LatestSeqNo returns the latest WAL sequence number.
func (w *WAL) LatestSeqNo() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.currentSeqNo
}

// CleanBefore removes WAL segments with sequence numbers before seqNo.
func (w *WAL) CleanBefore(seqNo int64) error {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		var sn int64
		if _, err := fmt.Sscanf(e.Name(), "wal-%016d.log", &sn); err == nil && sn < seqNo {
			os.Remove(filepath.Join(w.dir, e.Name()))
		}
	}
	return nil
}

// rotate closes the current segment and opens a new one.
func (w *WAL) rotate() error {
	if w.currentFile != nil {
		w.currentFile.Sync()
		w.currentFile.Close()
	}

	w.currentSeqNo++
	filename := filepath.Join(w.dir, fmt.Sprintf("wal-%016d.log", w.currentSeqNo))
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open WAL segment: %w", err)
	}

	w.currentFile = f
	w.currentSize = 0
	w.logger.Debug("WAL segment rotated", "seq", w.currentSeqNo, "file", filename)
	return nil
}

func (w *WAL) syncLoop(ctx context.Context, interval time.Duration) {
	defer w.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.mu.Lock()
			if w.currentFile != nil {
				w.currentFile.Sync()
			}
			w.mu.Unlock()
		}
	}
}

// WALReader reads WAL segments for replay (used by followers).
type WALReader struct {
	dir string
}

// NewWALReader creates a reader for WAL segments.
func NewWALReader(dir string) *WALReader {
	return &WALReader{dir: dir}
}

// ReadSegment reads all data points from a WAL segment file.
func (r *WALReader) ReadSegment(seqNo int64) ([]metrics.DataPoint, error) {
	filename := filepath.Join(r.dir, fmt.Sprintf("wal-%016d.log", seqNo))
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var allPoints []metrics.DataPoint
	for {
		var lenBuf [4]byte
		if _, err := io.ReadFull(f, lenBuf[:]); err != nil {
			if err == io.EOF {
				break
			}
			return allPoints, fmt.Errorf("read WAL length: %w", err)
		}
		dataLen := binary.BigEndian.Uint32(lenBuf[:])
		data := make([]byte, dataLen)
		if _, err := io.ReadFull(f, data); err != nil {
			return allPoints, fmt.Errorf("read WAL data: %w", err)
		}

		var points []metrics.DataPoint
		if err := json.Unmarshal(data, &points); err != nil {
			return allPoints, fmt.Errorf("unmarshal WAL entry: %w", err)
		}
		allPoints = append(allPoints, points...)
	}

	return allPoints, nil
}

// ListSegments returns available segment sequence numbers in order.
func (r *WALReader) ListSegments() ([]int64, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil, err
	}
	var seqNos []int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		var sn int64
		if _, err := fmt.Sscanf(e.Name(), "wal-%016d.log", &sn); err == nil {
			seqNos = append(seqNos, sn)
		}
	}
	sort.Slice(seqNos, func(i, j int) bool { return seqNos[i] < seqNos[j] })
	return seqNos, nil
}

// SegmentsAfter returns segment sequence numbers after the given sequence number.
func (r *WALReader) SegmentsAfter(afterSeqNo int64) ([]int64, error) {
	all, err := r.ListSegments()
	if err != nil {
		return nil, err
	}
	var result []int64
	for _, sn := range all {
		if sn > afterSeqNo {
			result = append(result, sn)
		}
	}
	return result, nil
}

// findLatestSeqNo finds the highest sequence number in the WAL directory.
func findLatestSeqNo(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var maxSeq int64
	for _, e := range entries {
		var sn int64
		if _, err := fmt.Sscanf(e.Name(), "wal-%016d.log", &sn); err == nil && sn > maxSeq {
			maxSeq = sn
		}
	}
	return maxSeq
}
