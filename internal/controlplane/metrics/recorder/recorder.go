// Package recorder provides an event-driven metrics recording interface that
// buffers data points in memory and periodically flushes them to a TSDB store.
package recorder

import (
	"context"
	"sync"
	"time"

	"github.com/vyprai/loka/internal/controlplane/metrics/tsdb"
	"github.com/vyprai/loka/internal/metrics"
)

const (
	flushInterval  = 1 * time.Second
	bufferCapacity = 500
)

// Recorder is an event-driven metrics recording interface called inline from managers.
type Recorder interface {
	// Inc increments a counter metric by 1.
	Inc(name string, labels ...metrics.Label)
	// Add adds a value to a counter metric.
	Add(name string, value float64, labels ...metrics.Label)
	// Set sets a gauge metric to a specific value.
	Set(name string, value float64, labels ...metrics.Label)
	// Observe records a histogram observation.
	Observe(name string, value float64, labels ...metrics.Label)
}

// recorder is the concrete implementation that buffers writes and flushes to a MetricsStore.
type recorder struct {
	store  tsdb.MetricsStore
	mu     sync.Mutex
	buffer []metrics.DataPoint
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a new Recorder backed by the given MetricsStore. If store is nil,
// a NopRecorder is returned. The caller must cancel the provided context or call
// the returned cancel function to stop the background flush goroutine.
func New(ctx context.Context, store tsdb.MetricsStore) (Recorder, context.CancelFunc) {
	if store == nil {
		return NopRecorder{}, func() {}
	}

	ctx, cancel := context.WithCancel(ctx)
	r := &recorder{
		store:  store,
		buffer: make([]metrics.DataPoint, 0, bufferCapacity),
		ctx:    ctx,
		cancel: cancel,
	}

	r.wg.Add(1)
	go r.flushLoop()

	return r, func() {
		cancel()
		r.wg.Wait()
	}
}

func (r *recorder) Inc(name string, labels ...metrics.Label) {
	r.record(name, metrics.Counter, 1, labels)
}

func (r *recorder) Add(name string, value float64, labels ...metrics.Label) {
	r.record(name, metrics.Counter, value, labels)
}

func (r *recorder) Set(name string, value float64, labels ...metrics.Label) {
	r.record(name, metrics.Gauge, value, labels)
}

func (r *recorder) Observe(name string, value float64, labels ...metrics.Label) {
	r.record(name, metrics.Histogram, value, labels)
}

func (r *recorder) record(name string, typ metrics.MetricType, value float64, labels []metrics.Label) {
	dp := metrics.DataPoint{
		Name:      name,
		Type:      typ,
		Labels:    labels,
		Timestamp: time.Now().UnixMilli(),
		Value:     value,
	}

	r.mu.Lock()
	r.buffer = append(r.buffer, dp)
	shouldFlush := len(r.buffer) >= bufferCapacity
	r.mu.Unlock()

	if shouldFlush {
		r.flush()
	}
}

// flush drains the buffer and writes all points to the store.
func (r *recorder) flush() {
	r.mu.Lock()
	if len(r.buffer) == 0 {
		r.mu.Unlock()
		return
	}
	points := r.buffer
	r.buffer = make([]metrics.DataPoint, 0, bufferCapacity)
	r.mu.Unlock()

	// Use a background context for the actual write since the flush may happen
	// after the parent context is cancelled (final drain).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = r.store.Write(ctx, points)
}

// flushLoop runs as a background goroutine, flushing the buffer every flushInterval.
func (r *recorder) flushLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.ctx.Done():
			// Final flush to drain any remaining points.
			r.flush()
			return
		case <-ticker.C:
			r.flush()
		}
	}
}

// NopRecorder is a no-op implementation of Recorder for use in tests or when metrics are disabled.
type NopRecorder struct{}

func (NopRecorder) Inc(string, ...metrics.Label)              {}
func (NopRecorder) Add(string, float64, ...metrics.Label)     {}
func (NopRecorder) Set(string, float64, ...metrics.Label)     {}
func (NopRecorder) Observe(string, float64, ...metrics.Label) {}
