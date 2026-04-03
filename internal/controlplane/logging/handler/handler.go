// Package handler provides a custom slog.Handler that writes log entries
// to both an inner handler (e.g. stdout) and a LogStore via LogWriter.
package handler

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vyprai/loka/internal/controlplane/logging"
)

// LogWriter is the interface for writing log entries to a store.
type LogWriter interface {
	Write(ctx context.Context, entries []logging.LogEntry) error
}

// Handler is a slog.Handler that wraps an inner handler and also writes
// log entries to a LogWriter in batches.
type Handler struct {
	inner      slog.Handler
	writer     LogWriter
	baseLabels map[string]string
	minLevel   slog.Level
	buffer     chan logging.LogEntry
	attrs      []slog.Attr
	group      string

	closeOnce sync.Once
	done      chan struct{}
}

const (
	batchSize    = 100
	batchTimeout = 100 * time.Millisecond
	bufferSize   = 4096
)

// New creates a Handler that delegates to inner and writes entries to writer.
// baseLabels are merged into every LogEntry's Labels.
// If writer is nil, the handler only delegates to inner.
func New(writer LogWriter, baseLabels map[string]string, inner slog.Handler) *Handler {
	h := &Handler{
		inner:      inner,
		writer:     writer,
		baseLabels: baseLabels,
		minLevel:   slog.LevelDebug,
		buffer:     make(chan logging.LogEntry, bufferSize),
		done:       make(chan struct{}),
	}
	if writer != nil {
		go h.flush()
	}
	return h
}

// Enabled reports whether the handler handles records at the given level.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	if level < h.minLevel {
		return false
	}
	return h.inner.Enabled(ctx, level)
}

// Handle writes the record to the inner handler and queues a LogEntry for the writer.
func (h *Handler) Handle(ctx context.Context, record slog.Record) error {
	err := h.inner.Handle(ctx, record)

	if h.writer == nil {
		return err
	}

	entry := h.recordToEntry(record)

	// Non-blocking send; drop if buffer is full.
	select {
	case h.buffer <- entry:
	default:
	}

	return err
}

// WithAttrs returns a new Handler with the given attrs accumulated.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{
		inner:      h.inner.WithAttrs(attrs),
		writer:     h.writer,
		baseLabels: h.baseLabels,
		minLevel:   h.minLevel,
		buffer:     h.buffer,
		attrs:      append(cloneAttrs(h.attrs), attrs...),
		group:      h.group,
		done:       h.done,
	}
}

// WithGroup returns a new Handler with the given group name.
func (h *Handler) WithGroup(name string) slog.Handler {
	newGroup := name
	if h.group != "" {
		newGroup = h.group + "." + name
	}
	return &Handler{
		inner:      h.inner.WithGroup(name),
		writer:     h.writer,
		baseLabels: h.baseLabels,
		minLevel:   h.minLevel,
		buffer:     h.buffer,
		attrs:      cloneAttrs(h.attrs),
		group:      newGroup,
		done:       h.done,
	}
}

// Close flushes any remaining buffered entries and stops the background goroutine.
func (h *Handler) Close() {
	h.closeOnce.Do(func() {
		close(h.buffer)
		if h.writer != nil {
			<-h.done
		}
	})
}

func (h *Handler) recordToEntry(record slog.Record) logging.LogEntry {
	level := strings.ToLower(record.Level.String())

	labels := make(map[string]string, len(h.baseLabels)+1)
	for k, v := range h.baseLabels {
		labels[k] = v
	}
	labels[logging.LabelLevel] = level

	fields := make(map[string]string)

	// Add accumulated attrs.
	for _, a := range h.attrs {
		key := a.Key
		if h.group != "" {
			key = h.group + "." + key
		}
		fields[key] = a.Value.String()
	}

	// Add record attrs.
	record.Attrs(func(a slog.Attr) bool {
		key := a.Key
		if h.group != "" {
			key = h.group + "." + key
		}
		fields[key] = a.Value.String()
		return true
	})

	return logging.LogEntry{
		Timestamp: record.Time,
		Level:     level,
		Message:   record.Message,
		Labels:    labels,
		Fields:    fields,
	}
}

func (h *Handler) flush() {
	defer close(h.done)

	batch := make([]logging.LogEntry, 0, batchSize)
	timer := time.NewTimer(batchTimeout)
	defer timer.Stop()

	for {
		select {
		case entry, ok := <-h.buffer:
			if !ok {
				// Channel closed — flush remaining.
				if len(batch) > 0 {
					h.writer.Write(context.Background(), batch)
				}
				return
			}
			batch = append(batch, entry)
			if len(batch) >= batchSize {
				h.writer.Write(context.Background(), batch)
				batch = batch[:0]
				timer.Reset(batchTimeout)
			}
		case <-timer.C:
			if len(batch) > 0 {
				h.writer.Write(context.Background(), batch)
				batch = batch[:0]
			}
			timer.Reset(batchTimeout)
		}
	}
}

func cloneAttrs(attrs []slog.Attr) []slog.Attr {
	if attrs == nil {
		return nil
	}
	c := make([]slog.Attr, len(attrs))
	copy(c, attrs)
	return c
}
