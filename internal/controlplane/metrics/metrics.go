package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Sessions
	SessionsTotal = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "loka",
		Name:      "sessions_total",
		Help:      "Total number of sessions by status.",
	}, []string{"status"})

	SessionsCreated = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "loka",
		Name:      "sessions_created_total",
		Help:      "Total sessions created.",
	})

	SessionsDestroyed = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "loka",
		Name:      "sessions_destroyed_total",
		Help:      "Total sessions destroyed.",
	})

	// Workers
	WorkersTotal = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "loka",
		Name:      "workers_total",
		Help:      "Total number of workers by status.",
	}, []string{"status", "provider"})

	// Executions
	ExecsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "loka",
		Name:      "executions_total",
		Help:      "Total command executions.",
	})

	ExecDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "loka",
		Name:      "execution_duration_seconds",
		Help:      "Command execution duration.",
		Buckets:   prometheus.ExponentialBuckets(0.01, 2, 15), // 10ms to ~5min
	})

	ExecsByStatus = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "loka",
		Name:      "executions_by_status_total",
		Help:      "Executions by final status.",
	}, []string{"status"})

	// Checkpoints
	CheckpointsCreated = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "loka",
		Name:      "checkpoints_created_total",
		Help:      "Total checkpoints created by type.",
	}, []string{"type"})

	CheckpointDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "loka",
		Name:      "checkpoint_duration_seconds",
		Help:      "Checkpoint creation duration.",
		Buckets:   prometheus.ExponentialBuckets(0.01, 2, 12),
	}, []string{"type"})

	CheckpointRestores = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "loka",
		Name:      "checkpoint_restores_total",
		Help:      "Total checkpoint restores.",
	})

	// API
	APIRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "loka",
		Name:      "api_requests_total",
		Help:      "Total API requests by method and path.",
	}, []string{"method", "path", "status"})

	APILatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "loka",
		Name:      "api_latency_seconds",
		Help:      "API request latency.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"method", "path"})

	// Databases
	DatabaseBackups = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "loka",
		Name:      "database_backups_total",
		Help:      "Total database backup operations by engine and status.",
	}, []string{"engine", "status"})

	DatabaseBackupDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "loka",
		Name:      "database_backup_duration_seconds",
		Help:      "Database backup duration.",
		Buckets:   []float64{1, 5, 15, 30, 60, 120, 300, 600},
	}, []string{"engine"})

	DatabaseCredentialRotations = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "loka",
		Name:      "database_credential_rotations_total",
		Help:      "Total credential rotation operations by engine.",
	}, []string{"engine"})

	DatabaseUpgrades = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "loka",
		Name:      "database_upgrades_total",
		Help:      "Total database version upgrades.",
	}, []string{"engine", "from_version", "to_version"})
)
