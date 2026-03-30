package loka

import "time"

// WorkerStatus represents the current state of a worker.
type WorkerStatus string

const (
	WorkerStatusRegistering WorkerStatus = "registering"
	WorkerStatusReady       WorkerStatus = "ready"
	WorkerStatusBusy        WorkerStatus = "busy"
	WorkerStatusDraining    WorkerStatus = "draining"
	WorkerStatusSuspect     WorkerStatus = "suspect"
	WorkerStatusDead        WorkerStatus = "dead"
)

// Worker represents a machine that runs LOKA microVMs.
type Worker struct {
	ID           string
	Hostname     string
	IPAddress    string // Public IP (or primary IP).
	PrivateIP    string // Private/internal IP for CP→worker traffic.
	Provider     string
	Region       string
	Zone         string
	Status       WorkerStatus
	Labels       map[string]string
	Capacity     ResourceCapacity
	AgentVersion string
	KVMAvailable bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastSeen     time.Time
}

// ResourceCapacity describes the total resources available on a worker.
type ResourceCapacity struct {
	CPUCores   int
	MemoryMB   int64
	DiskMB     int64
}

// Heartbeat carries periodic status and resource usage from a worker.
type Heartbeat struct {
	WorkerID      string
	Timestamp     time.Time
	Status        WorkerStatus
	Usage         ResourceUsage
	SessionCount  int
	SessionIDs    []string
}

// ResourceUsage reports current resource consumption on a worker.
type ResourceUsage struct {
	CPUPercent    float64
	MemoryUsedMB int64
	DiskUsedMB   int64
}
