package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/vyprai/loka/internal/controlplane/logging"
)

// LogStore is a BadgerDB-backed log storage engine.
type LogStore interface {
	// Write writes log entries to the store.
	Write(ctx context.Context, entries []logging.LogEntry) error

	// Query returns log entries matching the query request.
	Query(ctx context.Context, req logging.QueryRequest) (*logging.QueryResult, error)

	// QueryWithMatchers returns log entries for specific streams with filters.
	QueryWithMatchers(ctx context.Context, streamIDs []uint64, start, end time.Time, limit int, direction string, lineFilter func(string) bool) (*logging.QueryResult, error)

	// Tail returns a channel that receives matching log entries in real time.
	Tail(ctx context.Context, query string) (<-chan logging.LogEntry, error)

	// ListLabels returns all known label names.
	ListLabels(ctx context.Context) ([]string, error)

	// ListLabelValues returns all values for a given label name.
	ListLabelValues(ctx context.Context, labelName string) ([]string, error)

	// FindStreams returns stream IDs matching the given label matchers.
	FindStreams(ctx context.Context, matchers []LabelMatcher) ([]uint64, error)

	// GetStreamLabels returns the labels for a stream.
	GetStreamLabels(ctx context.Context, streamID uint64) (map[string]string, error)

	// Close closes the store.
	Close() error
}

// MatchType is the type of label matcher.
type MatchType int

const (
	MatchEqual    MatchType = iota // =
	MatchNotEqual                  // !=
	MatchRegexp                    // =~
	MatchNotRegexp                 // !~
)

// LabelMatcher matches labels by name/value with a given match type.
type LabelMatcher struct {
	Name  string
	Value string
	Type  MatchType
}

// StoreConfig configures the BadgerDB log store.
type StoreConfig struct {
	DataDir   string
	Retention time.Duration // TTL for log entries.
	Logger    *slog.Logger
}

// Stats holds self-monitoring counters for the log store.
type Stats struct {
	WriteEntriesTotal atomic.Int64
	WriteErrors       atomic.Int64
	QueryTotal        atomic.Int64
	QueryErrors       atomic.Int64
}

// subscriber is a tail subscriber.
type subscriber struct {
	ch     chan logging.LogEntry
	cancel context.CancelFunc
}

// store implements LogStore.
type store struct {
	db        *badger.DB
	retention time.Duration
	logger    *slog.Logger
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	stats     Stats

	subsMu sync.RWMutex
	subs   map[uint64]*subscriber // keyed by unique subscriber ID
	subSeq atomic.Uint64
}

// NewStore creates a new BadgerDB-backed LogStore.
func NewStore(cfg StoreConfig) (LogStore, error) {
	if cfg.Retention == 0 {
		cfg.Retention = 72 * time.Hour
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
	s := &store{
		db:        db,
		retention: cfg.Retention,
		logger:    cfg.Logger,
		cancel:    cancel,
		subs:      make(map[uint64]*subscriber),
	}

	// Start value log GC goroutine.
	s.wg.Add(1)
	go s.gcLoop(ctx)

	return s, nil
}

// Write writes log entries to BadgerDB with TTL and indexes.
func (s *store) Write(ctx context.Context, entries []logging.LogEntry) error {
	wb := s.db.NewWriteBatch()
	defer wb.Cancel()

	for i := range entries {
		entry := &entries[i]
		streamID := entry.StreamID()

		// 1. Log entry (gzip-compressed JSON).
		entryKey := EncodeLogEntryKey(streamID, entry.Timestamp.UnixNano())
		compressed, err := compressEntry(entry)
		if err != nil {
			return err
		}
		if err := wb.SetEntry(badger.NewEntry(entryKey, compressed).WithTTL(s.retention)); err != nil {
			return err
		}

		// 2. Stream index (store labels as JSON).
		labelsJSON, err := json.Marshal(entry.Labels)
		if err != nil {
			return err
		}
		streamKey := EncodeStreamKey(streamID)
		if err := wb.SetEntry(badger.NewEntry(streamKey, labelsJSON).WithTTL(s.retention)); err != nil {
			return err
		}

		// 3. Inverted label indexes.
		for k, v := range entry.Labels {
			invKey := EncodeInvertedKey(k, v, streamID)
			if err := wb.SetEntry(badger.NewEntry(invKey, nil).WithTTL(s.retention)); err != nil {
				return err
			}
		}

		// 4. Label name catalog.
		for k := range entry.Labels {
			lnKey := EncodeLabelNameKey(k)
			if err := wb.SetEntry(badger.NewEntry(lnKey, nil).WithTTL(s.retention + time.Hour)); err != nil {
				return err
			}
		}
	}

	if err := wb.Flush(); err != nil {
		s.stats.WriteErrors.Add(1)
		return err
	}
	s.stats.WriteEntriesTotal.Add(int64(len(entries)))

	// Fan out to tail subscribers.
	s.fanOut(entries)

	return nil
}

// Query returns log entries matching the query request.
// For now, the Query string is treated as a simple label selector: {key="value", ...}
// A full LogQL parser will be plugged in later.
func (s *store) Query(ctx context.Context, req logging.QueryRequest) (*logging.QueryResult, error) {
	s.stats.QueryTotal.Add(1)
	start := time.Now()

	matchers, lineFilter := parseSimpleQuery(req.Query)

	streamIDs, err := s.FindStreams(ctx, matchers)
	if err != nil {
		s.stats.QueryErrors.Add(1)
		return nil, err
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	direction := req.Direction
	if direction == "" {
		direction = "backward"
	}

	result, err := s.QueryWithMatchers(ctx, streamIDs, req.Start, req.End, limit, direction, lineFilter)
	if err != nil {
		s.stats.QueryErrors.Add(1)
		return nil, err
	}

	result.Stats.ExecutionMs = time.Since(start).Milliseconds()
	return result, nil
}

// QueryWithMatchers returns log entries for specific streams with filters.
func (s *store) QueryWithMatchers(ctx context.Context, streamIDs []uint64, start, end time.Time, limit int, direction string, lineFilter func(string) bool) (*logging.QueryResult, error) {
	if limit <= 0 {
		limit = 100
	}
	if direction == "" {
		direction = "backward"
	}

	startNs := start.UnixNano()
	endNs := end.UnixNano()

	streamMap := make(map[uint64]*logging.Stream)
	var stats logging.QueryStats
	totalCollected := 0

	err := s.db.View(func(txn *badger.Txn) error {
		for _, sid := range streamIDs {
			if totalCollected >= limit {
				break
			}

			prefix := LogEntryKeyPrefix(sid)
			seekKey := LogEntryKeyRangeStart(sid, startNs)

			opts := badger.DefaultIteratorOptions
			opts.Prefix = prefix
			if direction == "backward" {
				opts.Reverse = true
			}
			it := txn.NewIterator(opts)
			defer it.Close()

			if direction == "backward" {
				// For reverse iteration, seek to end of range.
				seekKey = EncodeLogEntryKey(sid, endNs)
			}

			for it.Seek(seekKey); it.Valid(); it.Next() {
				if totalCollected >= limit {
					break
				}

				item := it.Item()
				_, tsNs, ok := DecodeLogEntryKey(item.Key())
				if !ok {
					continue
				}

				if direction == "backward" {
					if tsNs < startNs {
						break
					}
				} else {
					if tsNs > endNs {
						break
					}
				}

				stats.EntriesScanned++

				err := item.Value(func(val []byte) error {
					stats.BytesProcessed += int64(len(val))

					entry, err := decompressEntry(val)
					if err != nil {
						return err
					}

					// Apply line filter.
					if lineFilter != nil && !lineFilter(entry.Message) {
						return nil
					}

					st, exists := streamMap[sid]
					if !exists {
						labels, _ := s.getStreamLabelsFromTxn(txn, sid)
						st = &logging.Stream{
							Labels:  labels,
							Entries: make([]logging.LogEntry, 0),
						}
						streamMap[sid] = st
					}

					st.Entries = append(st.Entries, *entry)
					totalCollected++
					return nil
				})
				if err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	streams := make([]logging.Stream, 0, len(streamMap))
	for _, st := range streamMap {
		streams = append(streams, *st)
	}

	return &logging.QueryResult{
		Streams: streams,
		Stats:   stats,
	}, nil
}

// Tail returns a channel that receives log entries in real time.
func (s *store) Tail(ctx context.Context, query string) (<-chan logging.LogEntry, error) {
	ch := make(chan logging.LogEntry, 256)

	subCtx, subCancel := context.WithCancel(ctx)
	subID := s.subSeq.Add(1)

	sub := &subscriber{
		ch:     ch,
		cancel: subCancel,
	}

	s.subsMu.Lock()
	s.subs[subID] = sub
	s.subsMu.Unlock()

	// Cleanup goroutine.
	go func() {
		<-subCtx.Done()
		s.subsMu.Lock()
		delete(s.subs, subID)
		s.subsMu.Unlock()
		close(ch)
	}()

	return ch, nil
}

// ListLabels returns all known label names.
func (s *store) ListLabels(ctx context.Context) ([]string, error) {
	var names []string
	prefix := LabelNameKeyPrefix()

	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			name, ok := DecodeLabelNameKey(it.Item().Key())
			if ok {
				names = append(names, name)
			}
		}
		return nil
	})

	return names, err
}

// ListLabelValues returns all values for a given label name.
func (s *store) ListLabelValues(ctx context.Context, labelName string) ([]string, error) {
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
	sort.Strings(values)
	return values, err
}

// FindStreams returns stream IDs matching all the given label matchers.
func (s *store) FindStreams(ctx context.Context, matchers []LabelMatcher) ([]uint64, error) {
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

// matchLabel finds all stream IDs matching a single label matcher.
func (s *store) matchLabel(txn *badger.Txn, m LabelMatcher) (map[uint64]struct{}, error) {
	matched := make(map[uint64]struct{})

	switch m.Type {
	case MatchEqual:
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

// GetStreamLabels returns the labels for a stream.
func (s *store) GetStreamLabels(ctx context.Context, streamID uint64) (map[string]string, error) {
	var labels map[string]string

	err := s.db.View(func(txn *badger.Txn) error {
		var err error
		labels, err = s.getStreamLabelsFromTxn(txn, streamID)
		return err
	})

	return labels, err
}

// getStreamLabelsFromTxn reads stream labels within an existing transaction.
func (s *store) getStreamLabelsFromTxn(txn *badger.Txn, streamID uint64) (map[string]string, error) {
	item, err := txn.Get(EncodeStreamKey(streamID))
	if err != nil {
		return nil, err
	}

	var labels map[string]string
	err = item.Value(func(val []byte) error {
		return json.Unmarshal(val, &labels)
	})
	return labels, err
}

// Close stops background goroutines and closes BadgerDB.
func (s *store) Close() error {
	// Cancel all tail subscribers.
	s.subsMu.Lock()
	for _, sub := range s.subs {
		sub.cancel()
	}
	s.subsMu.Unlock()

	s.cancel()
	s.wg.Wait()
	return s.db.Close()
}

// gcLoop runs BadgerDB value log GC periodically.
func (s *store) gcLoop(ctx context.Context) {
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

// fanOut sends entries to all tail subscribers.
func (s *store) fanOut(entries []logging.LogEntry) {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()

	for _, sub := range s.subs {
		for _, entry := range entries {
			select {
			case sub.ch <- entry:
			default:
				// Drop if subscriber is slow.
			}
		}
	}
}

// compressEntry gzip-compresses a LogEntry as JSON.
func compressEntry(entry *logging.LogEntry) ([]byte, error) {
	jsonData, err := json.Marshal(entry)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(jsonData); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decompressEntry decompresses a gzip-compressed JSON LogEntry.
func decompressEntry(data []byte) (*logging.LogEntry, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	jsonData, err := io.ReadAll(gz)
	if err != nil {
		return nil, err
	}

	var entry logging.LogEntry
	if err := json.Unmarshal(jsonData, &entry); err != nil {
		return nil, err
	}
	return &entry, nil
}

// compileRegexp compiles a regex, anchoring it to match the full string.
func compileRegexp(pattern string) (*regexp.Regexp, error) {
	return regexp.Compile("^(?:" + pattern + ")$")
}

// parseSimpleQuery parses a simple label selector query like {key="value", key2="value2"}.
// Returns matchers and a nil line filter. A full LogQL parser will replace this.
func parseSimpleQuery(query string) ([]LabelMatcher, func(string) bool) {
	if query == "" {
		return nil, nil
	}

	// Strip outer braces.
	q := query
	if len(q) >= 2 && q[0] == '{' && q[len(q)-1] == '}' {
		q = q[1 : len(q)-1]
	}

	if q == "" {
		return nil, nil
	}

	var matchers []LabelMatcher

	// Split on commas, parse each matcher.
	parts := splitMatchers(q)
	for _, part := range parts {
		m, ok := parseMatcher(part)
		if ok {
			matchers = append(matchers, m)
		}
	}

	return matchers, nil
}

// splitMatchers splits a comma-separated list of matchers, respecting quoted values.
func splitMatchers(s string) []string {
	var parts []string
	var current []byte
	inQuote := false

	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '"' {
			inQuote = !inQuote
			current = append(current, ch)
		} else if ch == ',' && !inQuote {
			parts = append(parts, string(current))
			current = current[:0]
		} else {
			current = append(current, ch)
		}
	}
	if len(current) > 0 {
		parts = append(parts, string(current))
	}
	return parts
}

// parseMatcher parses a single matcher like key="value" or key=~"pattern".
func parseMatcher(s string) (LabelMatcher, bool) {
	s = trimSpace(s)

	// Try operators in order of specificity.
	for _, op := range []struct {
		sep  string
		typ  MatchType
	}{
		{"!~", MatchNotRegexp},
		{"=~", MatchRegexp},
		{"!=", MatchNotEqual},
		{"=", MatchEqual},
	} {
		idx := findOperator(s, op.sep)
		if idx >= 0 {
			name := trimSpace(s[:idx])
			value := trimSpace(s[idx+len(op.sep):])
			// Strip quotes from value.
			if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
				value = value[1 : len(value)-1]
			}
			return LabelMatcher{Name: name, Value: value, Type: op.typ}, true
		}
	}

	return LabelMatcher{}, false
}

// findOperator finds the first occurrence of an operator, avoiding false matches
// (e.g., "!=" should not match "=" first).
func findOperator(s, op string) int {
	for i := 0; i <= len(s)-len(op); i++ {
		if s[i:i+len(op)] == op {
			// For single "=", make sure it's not part of "!=", "=~".
			if op == "=" && i > 0 && (s[i-1] == '!' || s[i-1] == '=') {
				continue
			}
			if op == "=" && i+1 < len(s) && s[i+1] == '~' {
				continue
			}
			return i
		}
	}
	return -1
}

// trimSpace trims whitespace without importing strings.
func trimSpace(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
