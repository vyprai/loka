package tsdb

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/vyprai/loka/internal/metrics"
)

// MetricsStore is a BadgerDB-backed time-series database.
type MetricsStore interface {
	// Write writes data points to the store.
	Write(ctx context.Context, points []metrics.DataPoint) error

	// Query returns data points for the given series within the time range.
	Query(ctx context.Context, req QueryRequest) ([]QueryResult, error)

	// ListMetrics returns all known metric names.
	ListMetrics(ctx context.Context) ([]string, error)

	// ListLabelNames returns all known label names.
	ListLabelNames(ctx context.Context) ([]string, error)

	// ListLabelValues returns all values for a given label name.
	ListLabelValues(ctx context.Context, labelName string) ([]string, error)

	// FindSeries returns series IDs matching the given label matchers.
	FindSeries(ctx context.Context, matchers []LabelMatcher) ([]uint64, error)

	// GetSeriesInfo returns metadata for a series.
	GetSeriesInfo(ctx context.Context, seriesID uint64) (*metrics.SeriesInfo, error)

	// GetStats returns self-monitoring counters.
	GetStats() *Stats

	// DiskSize returns the approximate disk usage (LSM size, value log size).
	DiskSize() (int64, int64)

	// Close closes the store.
	Close() error
}

// StoreConfig configures the BadgerDB metrics store.
type StoreConfig struct {
	DataDir   string
	Retention time.Duration // TTL for raw data points.
	Logger    *slog.Logger
}

// Stats holds self-monitoring counters for the TSDB.
type Stats struct {
	WriteSamplesTotal atomic.Int64
	WriteErrors       atomic.Int64
	QueryTotal        atomic.Int64
	QueryErrors       atomic.Int64
}

// badgerStore implements MetricsStore.
type badgerStore struct {
	db        *badger.DB
	retention time.Duration
	logger    *slog.Logger
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	stats     Stats
}

// NewStore creates a new BadgerDB-backed MetricsStore.
func NewStore(cfg StoreConfig) (MetricsStore, error) {
	if cfg.Retention == 0 {
		cfg.Retention = 48 * time.Hour
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	opts := badger.DefaultOptions(cfg.DataDir).
		WithLogger(nil). // suppress badger's own logging
		WithValueLogFileSize(64 << 20).
		WithNumMemtables(2)

	db, err := badger.Open(opts)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &badgerStore{
		db:        db,
		retention: cfg.Retention,
		logger:    cfg.Logger,
		cancel:    cancel,
	}

	// Start value log GC goroutine.
	s.wg.Add(1)
	go s.gcLoop(ctx)

	return s, nil
}

// GetStats returns the self-monitoring stats for the TSDB.
func (s *badgerStore) GetStats() *Stats {
	return &s.stats
}

// DiskSize returns the approximate disk usage of the BadgerDB LSM + value log.
func (s *badgerStore) DiskSize() (int64, int64) {
	lsm, vlog := s.db.Size()
	return lsm, vlog
}

// Write writes data points to BadgerDB with TTL and indexes.
func (s *badgerStore) Write(ctx context.Context, points []metrics.DataPoint) error {
	wb := s.db.NewWriteBatch()
	defer wb.Cancel()

	for i := range points {
		dp := &points[i]
		dp.Labels.Sort()
		seriesID := dp.SeriesID()

		// 1. Data point.
		dpKey := EncodeDataPointKey(seriesID, dp.Timestamp)
		dpVal := EncodeFloat64(dp.Value)
		if err := wb.SetEntry(badger.NewEntry(dpKey, dpVal).WithTTL(s.retention)); err != nil {
			return err
		}

		// 2. Series index.
		info := metrics.SeriesInfo{
			Name:   dp.Name,
			Type:   dp.Type,
			Labels: dp.Labels.Map(),
		}
		infoBytes, err := json.Marshal(info)
		if err != nil {
			return err
		}
		seriesKey := EncodeSeriesKey(seriesID)
		if err := wb.SetEntry(badger.NewEntry(seriesKey, infoBytes).WithTTL(s.retention)); err != nil {
			return err
		}

		// 3. Inverted label indexes.
		// Add __name__ as a virtual label.
		nameKey := EncodeInvertedKey("__name__", dp.Name, seriesID)
		if err := wb.SetEntry(badger.NewEntry(nameKey, nil).WithTTL(s.retention)); err != nil {
			return err
		}
		for _, l := range dp.Labels {
			invKey := EncodeInvertedKey(l.Name, l.Value, seriesID)
			if err := wb.SetEntry(badger.NewEntry(invKey, nil).WithTTL(s.retention)); err != nil {
				return err
			}
		}

		// 4. Metric name index.
		mnKey := EncodeMetricNameKey(dp.Name)
		if err := wb.SetEntry(badger.NewEntry(mnKey, []byte{byte(dp.Type)}).WithTTL(s.retention + time.Hour)); err != nil {
			return err
		}
	}

	if err := wb.Flush(); err != nil {
		s.stats.WriteErrors.Add(1)
		return err
	}
	s.stats.WriteSamplesTotal.Add(int64(len(points)))
	return nil
}

// Query returns data points for the given series within the time range.
func (s *badgerStore) Query(ctx context.Context, req QueryRequest) ([]QueryResult, error) {
	results := make([]QueryResult, 0, len(req.SeriesIDs))

	startMs := req.Start.UnixMilli()
	endMs := req.End.UnixMilli()

	err := s.db.View(func(txn *badger.Txn) error {
		for _, sid := range req.SeriesIDs {
			qr := QueryResult{SeriesID: sid}
			prefix := DataPointKeyPrefix(sid)
			seekKey := DataPointKeyRangeStart(sid, startMs)

			opts := badger.DefaultIteratorOptions
			opts.Prefix = prefix
			it := txn.NewIterator(opts)
			defer it.Close()

			for it.Seek(seekKey); it.Valid(); it.Next() {
				item := it.Item()
				_, ts, ok := DecodeDataPointKey(item.Key())
				if !ok || ts > endMs {
					break
				}

				err := item.Value(func(val []byte) error {
					if len(val) == 8 {
						qr.Points = append(qr.Points, TimeValue{
							TimestampMs: ts,
							Value:       DecodeFloat64(val),
						})
					}
					return nil
				})
				if err != nil {
					return err
				}
			}

			if len(qr.Points) > 0 {
				results = append(results, qr)
			}
		}
		return nil
	})

	if req.Step > 0 && err == nil {
		for i := range results {
			results[i].Points = downsample(results[i].Points, startMs, endMs, req.Step)
		}
	}

	s.stats.QueryTotal.Add(1)
	if err != nil {
		s.stats.QueryErrors.Add(1)
	}
	return results, err
}

// downsample aggregates points into step-sized buckets using average.
func downsample(points []TimeValue, startMs, endMs int64, step time.Duration) []TimeValue {
	if len(points) == 0 {
		return points
	}

	stepMs := step.Milliseconds()
	var result []TimeValue

	for bucketStart := startMs; bucketStart <= endMs; bucketStart += stepMs {
		bucketEnd := bucketStart + stepMs
		var sum float64
		var count int

		for _, p := range points {
			if p.TimestampMs >= bucketStart && p.TimestampMs < bucketEnd {
				sum += p.Value
				count++
			}
		}

		if count > 0 {
			result = append(result, TimeValue{
				TimestampMs: bucketStart,
				Value:       sum / float64(count),
			})
		}
	}

	return result
}

// ListMetrics returns all known metric names.
func (s *badgerStore) ListMetrics(ctx context.Context) ([]string, error) {
	var names []string
	prefix := MetricNameKeyPrefix()

	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			name, ok := DecodeMetricNameKey(it.Item().Key())
			if ok {
				names = append(names, name)
			}
		}
		return nil
	})

	return names, err
}

// ListLabelNames returns all known label names by scanning the inverted index.
func (s *badgerStore) ListLabelNames(ctx context.Context) ([]string, error) {
	seen := make(map[string]struct{})
	prefix := []byte{prefixInverted}

	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			labelName, _, _, ok := DecodeInvertedKey(it.Item().Key())
			if ok {
				if _, exists := seen[labelName]; !exists {
					seen[labelName] = struct{}{}
				}
			}
		}
		return nil
	})

	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	return names, err
}

// ListLabelValues returns all values for a given label name.
func (s *badgerStore) ListLabelValues(ctx context.Context, labelName string) ([]string, error) {
	seen := make(map[string]struct{})
	prefix := InvertedKeyPrefixLabel(labelName)

	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			name, value, _, ok := DecodeInvertedKey(it.Item().Key())
			if ok && name == labelName {
				seen[value] = struct{}{}
			}
		}
		return nil
	})

	values := make([]string, 0, len(seen))
	for v := range seen {
		values = append(values, v)
	}
	return values, err
}

// FindSeries returns series IDs matching all the given label matchers.
func (s *badgerStore) FindSeries(ctx context.Context, matchers []LabelMatcher) ([]uint64, error) {
	if len(matchers) == 0 {
		return nil, nil
	}

	var sets []map[uint64]struct{}

	err := s.db.View(func(txn *badger.Txn) error {
		for _, m := range matchers {
			matched, err := s.matchLabel(txn, m)
			if err != nil {
				return err
			}
			sets = append(sets, matched)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Intersect all sets.
	result := sets[0]
	for i := 1; i < len(sets); i++ {
		intersected := make(map[uint64]struct{})
		for id := range result {
			if _, ok := sets[i][id]; ok {
				intersected[id] = struct{}{}
			}
		}
		result = intersected
	}

	ids := make([]uint64, 0, len(result))
	for id := range result {
		ids = append(ids, id)
	}
	return ids, nil
}

// matchLabel finds all series IDs matching a single label matcher.
func (s *badgerStore) matchLabel(txn *badger.Txn, m LabelMatcher) (map[uint64]struct{}, error) {
	matched := make(map[uint64]struct{})

	switch m.Type {
	case MatchEqual:
		// Exact match: scan prefix [label_name][label_value]
		prefix := InvertedKeyPrefixLabelValue(m.Name, m.Value)
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			_, _, sid, ok := DecodeInvertedKey(it.Item().Key())
			if ok {
				matched[sid] = struct{}{}
			}
		}

	case MatchNotEqual:
		// Scan all values for this label name, exclude matching value.
		prefix := InvertedKeyPrefixLabel(m.Name)
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			name, value, sid, ok := DecodeInvertedKey(it.Item().Key())
			if ok && name == m.Name && value != m.Value {
				matched[sid] = struct{}{}
			}
		}

	case MatchRegexp:
		// Scan all values, include regex matches.
		re, err := compileRegexp(m.Value)
		if err != nil {
			return nil, err
		}
		prefix := InvertedKeyPrefixLabel(m.Name)
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			name, value, sid, ok := DecodeInvertedKey(it.Item().Key())
			if ok && name == m.Name && re.MatchString(value) {
				matched[sid] = struct{}{}
			}
		}

	case MatchNotRegexp:
		// Scan all values, exclude regex matches.
		re, err := compileRegexp(m.Value)
		if err != nil {
			return nil, err
		}
		prefix := InvertedKeyPrefixLabel(m.Name)
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			name, value, sid, ok := DecodeInvertedKey(it.Item().Key())
			if ok && name == m.Name && !re.MatchString(value) {
				matched[sid] = struct{}{}
			}
		}
	}

	return matched, nil
}

// GetSeriesInfo returns metadata for a series.
func (s *badgerStore) GetSeriesInfo(ctx context.Context, seriesID uint64) (*metrics.SeriesInfo, error) {
	var info metrics.SeriesInfo

	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(EncodeSeriesKey(seriesID))
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			return json.Unmarshal(val, &info)
		})
	})
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// Close stops background goroutines and closes BadgerDB.
func (s *badgerStore) Close() error {
	s.cancel()
	s.wg.Wait()
	return s.db.Close()
}

// gcLoop runs BadgerDB value log GC periodically.
func (s *badgerStore) gcLoop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for {
				err := s.db.RunValueLogGC(0.5)
				if err != nil {
					break
				}
			}
		}
	}
}

// compileRegexp compiles a regex, anchoring it to match the full string.
func compileRegexp(pattern string) (*regexp.Regexp, error) {
	return regexp.Compile("^(?:" + pattern + ")$")
}

// InvertedKeyPrefixAll returns just the inverted index prefix byte for scanning all entries.
func InvertedKeyPrefixAll() []byte {
	return []byte{prefixInverted}
}

// SeriesKeyPrefix returns just the series prefix byte.
func SeriesKeyPrefix() []byte {
	return []byte{prefixSeries}
}

// hasPrefix checks if b starts with prefix.
func hasPrefix(b, prefix []byte) bool {
	return bytes.HasPrefix(b, prefix)
}
