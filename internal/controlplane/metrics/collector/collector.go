package collector

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/vyprai/loka/internal/controlplane/metrics/tsdb"
	"github.com/vyprai/loka/internal/metrics"
	"github.com/vyprai/loka/internal/store"
)

// Collector periodically polls entity counts from the SQL store
// and writes gauge-style metrics into the TSDB.
type Collector struct {
	store    store.Store
	tsdb     tsdb.MetricsStore
	interval time.Duration
	logger   *slog.Logger
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// New creates a new Collector.
func New(db store.Store, metricsStore tsdb.MetricsStore, interval time.Duration, logger *slog.Logger) *Collector {
	if interval == 0 {
		interval = 15 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Collector{
		store:    db,
		tsdb:     metricsStore,
		interval: interval,
		logger:   logger,
	}
}

// Start begins the collection loop. Call Stop to shut down.
func (c *Collector) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.wg.Add(1)
	go c.run(ctx)
}

// Stop stops the collection loop.
func (c *Collector) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
	c.wg.Wait()
}

func (c *Collector) run(ctx context.Context) {
	defer c.wg.Done()
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	// Collect immediately on start.
	c.collect(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.collect(ctx)
		}
	}
}

func (c *Collector) collect(ctx context.Context) {
	now := time.Now().UnixMilli()
	var points []metrics.DataPoint

	// Sessions by status.
	sessions, err := c.store.Sessions().List(ctx, store.SessionFilter{Limit: 10000})
	if err != nil {
		c.logger.Warn("collector: failed to list sessions", "error", err)
	} else {
		counts := make(map[string]int)
		for _, s := range sessions {
			counts[string(s.Status)]++
		}
		for status, count := range counts {
			points = append(points, metrics.DataPoint{
				Name:      "sessions_total",
				Type:      metrics.Gauge,
				Labels:    metrics.Labels{{Name: "type", Value: "session"}, {Name: "status", Value: status}},
				Timestamp: now,
				Value:     float64(count),
			})
		}
	}

	// Services by status.
	services, _, err := c.store.Services().List(ctx, store.ServiceFilter{Limit: 10000})
	if err != nil {
		c.logger.Warn("collector: failed to list services", "error", err)
	} else {
		counts := make(map[string]int)
		for _, s := range services {
			counts[string(s.Status)]++
		}
		for status, count := range counts {
			points = append(points, metrics.DataPoint{
				Name:      "services_total",
				Type:      metrics.Gauge,
				Labels:    metrics.Labels{{Name: "type", Value: "service"}, {Name: "status", Value: status}},
				Timestamp: now,
				Value:     float64(count),
			})
		}
	}

	// Tasks by status.
	tasks, err := c.store.Tasks().List(ctx, store.TaskFilter{Limit: 10000})
	if err != nil {
		c.logger.Warn("collector: failed to list tasks", "error", err)
	} else {
		counts := make(map[string]int)
		for _, t := range tasks {
			counts[string(t.Status)]++
		}
		for status, count := range counts {
			points = append(points, metrics.DataPoint{
				Name:      "tasks_total",
				Type:      metrics.Gauge,
				Labels:    metrics.Labels{{Name: "type", Value: "task"}, {Name: "status", Value: status}},
				Timestamp: now,
				Value:     float64(count),
			})
		}
	}

	// Workers by status and provider.
	workers, err := c.store.Workers().List(ctx, store.WorkerFilter{Limit: 10000})
	if err != nil {
		c.logger.Warn("collector: failed to list workers", "error", err)
	} else {
		type workerKey struct{ status, provider, region string }
		counts := make(map[workerKey]int)
		for _, w := range workers {
			counts[workerKey{string(w.Status), w.Provider, w.Region}]++
		}
		for k, count := range counts {
			points = append(points, metrics.DataPoint{
				Name: "workers_total",
				Type: metrics.Gauge,
				Labels: metrics.Labels{
					{Name: "type", Value: "worker"},
					{Name: "status", Value: k.status},
					{Name: "provider", Value: k.provider},
					{Name: "region", Value: k.region},
				},
				Timestamp: now,
				Value:     float64(count),
			})
		}
	}

	// Volumes by type and status.
	volumes, err := c.store.Volumes().List(ctx)
	if err != nil {
		c.logger.Warn("collector: failed to list volumes", "error", err)
	} else {
		type volKey struct{ volType, status string }
		counts := make(map[volKey]int)
		for _, v := range volumes {
			counts[volKey{string(v.Type), string(v.Status)}]++
		}
		for k, count := range counts {
			points = append(points, metrics.DataPoint{
				Name: "volumes_total",
				Type: metrics.Gauge,
				Labels: metrics.Labels{
					{Name: "volume_type", Value: k.volType},
					{Name: "status", Value: k.status},
				},
				Timestamp: now,
				Value:     float64(count),
			})
		}
	}

	// Self-monitoring: TSDB stats.
	if stats := c.tsdb.GetStats(); stats != nil {
		points = append(points,
			metrics.DataPoint{Name: "loka_tsdb_write_samples_total", Type: metrics.Counter, Timestamp: now, Value: float64(stats.WriteSamplesTotal.Load())},
			metrics.DataPoint{Name: "loka_tsdb_write_errors_total", Type: metrics.Counter, Timestamp: now, Value: float64(stats.WriteErrors.Load())},
			metrics.DataPoint{Name: "loka_tsdb_query_total", Type: metrics.Counter, Timestamp: now, Value: float64(stats.QueryTotal.Load())},
			metrics.DataPoint{Name: "loka_tsdb_query_errors_total", Type: metrics.Counter, Timestamp: now, Value: float64(stats.QueryErrors.Load())},
		)
	}
	lsmSize, vlogSize := c.tsdb.DiskSize()
	points = append(points,
		metrics.DataPoint{Name: "loka_tsdb_disk_bytes", Type: metrics.Gauge, Timestamp: now, Value: float64(lsmSize + vlogSize)},
	)

	if len(points) > 0 {
		if err := c.tsdb.Write(ctx, points); err != nil {
			c.logger.Warn("collector: failed to write metrics", "error", err)
		}
	}
}
