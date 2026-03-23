package loka

import "time"

// SessionStatus represents the current state of a LOKA session.
type SessionStatus string

const (
	SessionStatusCreating    SessionStatus = "creating"
	SessionStatusRunning     SessionStatus = "running"
	SessionStatusPaused      SessionStatus = "paused"
	SessionStatusTerminating SessionStatus = "terminating"
	SessionStatusTerminated  SessionStatus = "terminated"
	SessionStatusError       SessionStatus = "error"
)

// Session represents a single LOKA microVM session.
type Session struct {
	ID         string
	Name       string
	Status     SessionStatus
	Mode       ExecMode
	WorkerID   string
	ImageRef   string // Docker image reference: "ubuntu:22.04", "python:3.12-slim"
	ImageID    string // Resolved image ID after pull.
	SnapshotID string // Optional: restore from this snapshot (diff on top of image).
	VCPUs      int
	MemoryMB   int
	Labels     map[string]string
	Mounts     []StorageMount `json:"Mounts,omitempty"` // Object storage mounts.
	Ports      []PortMapping  `json:"Ports,omitempty"`  // Port forwarding declarations.
	ExecPolicy ExecPolicy     `json:"ExecPolicy"`       // Command/package restrictions.
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ValidTransitions defines the allowed state transitions for a session.
var ValidSessionTransitions = map[SessionStatus][]SessionStatus{
	SessionStatusCreating:    {SessionStatusRunning, SessionStatusError},
	SessionStatusRunning:     {SessionStatusPaused, SessionStatusTerminating, SessionStatusError},
	SessionStatusPaused:      {SessionStatusRunning, SessionStatusTerminating, SessionStatusError},
	SessionStatusTerminating: {SessionStatusTerminated, SessionStatusError},
	SessionStatusError:       {SessionStatusTerminating},
}

// CanTransitionTo checks if the session can transition to the given status.
func (s *Session) CanTransitionTo(target SessionStatus) bool {
	allowed, ok := ValidSessionTransitions[s.Status]
	if !ok {
		return false
	}
	for _, a := range allowed {
		if a == target {
			return true
		}
	}
	return false
}
