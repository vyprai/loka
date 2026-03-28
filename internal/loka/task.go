package loka

import "time"

// TaskStatus represents the current state of a LOKA task.
type TaskStatus string

const (
	TaskStatusPending TaskStatus = "pending"
	TaskStatusRunning TaskStatus = "running"
	TaskStatusSuccess TaskStatus = "success"
	TaskStatusFailed  TaskStatus = "failed"
	TaskStatusError   TaskStatus = "error"
)

// Task is a one-time job that runs to completion in a microVM.
// Unlike services (long-running, auto-restart), tasks exit after execution
// and report success/failure based on exit code.
type Task struct {
	ID            string            `json:"ID"`
	Name          string            `json:"Name"`
	Status        TaskStatus        `json:"Status"`
	ExitCode      int               `json:"ExitCode"`
	WorkerID      string            `json:"WorkerID"`
	ImageRef      string            `json:"ImageRef"`
	Command       string            `json:"Command"`
	Args          []string          `json:"Args"`
	Env           map[string]string `json:"Env"`
	Workdir       string            `json:"Workdir"`
	BundleKey     string            `json:"BundleKey,omitempty"`
	VCPUs         int               `json:"VCPUs"`
	MemoryMB      int               `json:"MemoryMB"`
	Mounts        []Volume          `json:"Mounts,omitempty"`
	Timeout       int               `json:"Timeout,omitempty"` // Max duration in seconds (0 = no limit).
	StatusMessage string            `json:"StatusMessage,omitempty"`
	StartedAt     time.Time         `json:"StartedAt"`
	CompletedAt   time.Time         `json:"CompletedAt"`
	CreatedAt     time.Time         `json:"CreatedAt"`
	UpdatedAt     time.Time         `json:"UpdatedAt"`
}

// Duration returns the task's execution duration.
func (t *Task) Duration() time.Duration {
	if t.StartedAt.IsZero() {
		return 0
	}
	end := t.CompletedAt
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(t.StartedAt)
}
