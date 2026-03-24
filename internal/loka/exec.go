package loka

import "time"

// ExecStatus represents the state of a command execution.
type ExecStatus string

const (
	ExecStatusPending         ExecStatus = "pending"
	ExecStatusPendingApproval ExecStatus = "pending_approval" // Waiting for agent to approve (ask mode).
	ExecStatusRunning         ExecStatus = "running"
	ExecStatusSuccess         ExecStatus = "success"
	ExecStatusFailed          ExecStatus = "failed"
	ExecStatusCanceled        ExecStatus = "canceled"
	ExecStatusRejected        ExecStatus = "rejected" // Agent rejected the command in ask mode.
)

// Command represents a single command to execute inside a session VM.
type Command struct {
	ID       string
	Command  string
	Args     []string
	Workdir  string
	Env      map[string]string
	CPULimit int   // millicores, 0 = unlimited
	MemLimit int64 // bytes, 0 = unlimited
}

// CommandResult holds the output of a completed command.
type CommandResult struct {
	CommandID string
	ExitCode  int
	Stdout    string
	Stderr    string
	StartedAt time.Time
	EndedAt   time.Time
}

// Execution represents a single or parallel command execution within a session.
type Execution struct {
	ID        string
	SessionID string
	Status    ExecStatus
	Parallel  bool
	Commands  []Command
	Results   []CommandResult
	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsTerminal returns true if the execution is in a final state.
func (e *Execution) IsTerminal() bool {
	switch e.Status {
	case ExecStatusSuccess, ExecStatusFailed, ExecStatusCanceled, ExecStatusRejected:
		return true
	}
	return false
}
