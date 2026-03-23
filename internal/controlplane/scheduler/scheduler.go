package scheduler

import (
	"fmt"
	"sort"

	"github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/loka"
)

// Strategy defines the scheduling strategy.
type Strategy string

const (
	// StrategySpread distributes sessions across workers for availability.
	StrategySpread Strategy = "spread"
	// StrategyBinPack fills workers to capacity before using new ones (cost optimization).
	StrategyBinPack Strategy = "binpack"
)

// Constraints are optional scheduling constraints from the session request.
type Constraints struct {
	RequireLabels map[string]string // Worker must have these labels.
	PreferRegion  string            // Prefer workers in this region.
	PreferProvider string           // Prefer this cloud provider.
	ExcludeWorkers []string         // Workers to exclude (e.g., during rescheduling).
}

// Scheduler selects workers for session placement.
type Scheduler struct {
	registry *worker.Registry
	strategy Strategy
}

// New creates a new scheduler.
func New(registry *worker.Registry, strategy Strategy) *Scheduler {
	if strategy == "" {
		strategy = StrategySpread
	}
	return &Scheduler{registry: registry, strategy: strategy}
}

// Pick selects the best worker for a new session.
func (s *Scheduler) Pick(constraints Constraints) (*worker.WorkerConn, error) {
	candidates := s.registry.List()
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no workers available")
	}

	// Filter.
	var filtered []*worker.WorkerConn
	for _, c := range candidates {
		if !isEligible(c.Worker, constraints) {
			continue
		}
		filtered = append(filtered, c)
	}

	if len(filtered) == 0 {
		return nil, fmt.Errorf("no workers match constraints")
	}

	// Score and rank.
	scored := make([]scoredWorker, len(filtered))
	for i, c := range filtered {
		scored[i] = scoredWorker{
			conn:  c,
			score: s.score(c, constraints),
		}
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score // Higher score = better.
	})

	return scored[0].conn, nil
}

type scoredWorker struct {
	conn  *worker.WorkerConn
	score float64
}

func (s *Scheduler) score(c *worker.WorkerConn, constraints Constraints) float64 {
	w := c.Worker
	var score float64

	// Base score: worker status.
	switch w.Status {
	case loka.WorkerStatusReady:
		score += 100
	case loka.WorkerStatusBusy:
		score += 50
	default:
		return 0 // Not schedulable.
	}

	// Strategy-specific scoring.
	hb := heartbeatFromWorker(w)
	usedRatio := float64(hb.SessionCount) / float64(max(w.Capacity.CPUCores, 1))

	switch s.strategy {
	case StrategySpread:
		// Prefer workers with fewer sessions (lower utilization = higher score).
		score += (1.0 - usedRatio) * 50

	case StrategyBinPack:
		// Prefer workers with more sessions (higher utilization = higher score).
		// But not completely full.
		if usedRatio < 0.95 {
			score += usedRatio * 50
		}
	}

	// Region affinity bonus.
	if constraints.PreferRegion != "" && w.Region == constraints.PreferRegion {
		score += 20
	}

	// Provider affinity bonus.
	if constraints.PreferProvider != "" && w.Provider == constraints.PreferProvider {
		score += 10
	}

	// Capacity bonus — more resources available = higher score.
	score += float64(w.Capacity.CPUCores) * 0.5
	score += float64(w.Capacity.MemoryMB) / 1024.0

	return score
}

func isEligible(w *loka.Worker, c Constraints) bool {
	// Must be ready or busy (not draining, dead, etc.).
	if w.Status != loka.WorkerStatusReady && w.Status != loka.WorkerStatusBusy {
		return false
	}

	// Check exclusion list.
	for _, excluded := range c.ExcludeWorkers {
		if w.ID == excluded {
			return false
		}
	}

	// Check required labels.
	for k, v := range c.RequireLabels {
		if w.Labels[k] != v {
			return false
		}
	}

	return true
}

// heartbeatFromWorker creates a minimal heartbeat from worker state.
// In production, we'd use the latest heartbeat data from the registry.
func heartbeatFromWorker(w *loka.Worker) loka.Heartbeat {
	return loka.Heartbeat{
		WorkerID:     w.ID,
		SessionCount: 0, // Will be populated from registry in production.
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
