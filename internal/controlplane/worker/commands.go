package worker

import "github.com/vyprai/loka/internal/loka"

// CreateCheckpointData is the payload for creating a checkpoint on a worker.
type CreateCheckpointData struct {
	SessionID    string
	CheckpointID string
	Type         loka.CheckpointType
}

// RestoreCheckpointData is the payload for restoring a checkpoint on a worker.
type RestoreCheckpointData struct {
	SessionID    string
	CheckpointID string
	OverlayKey   string
}

// ApproveExecData is the payload for approving commands on the worker proxy.
type ApproveExecData struct {
	SessionID  string
	ExecID     string
	CommandIDs []string       // Command IDs to approve on the proxy.
	Commands   []loka.Command // Commands to re-execute after approval.
	Parallel   bool
}

// AddToWhitelistData is the payload for permanently whitelisting a command.
type AddToWhitelistData struct {
	SessionID string
	Command   string
}

// ApproveOnGateData resumes a suspended command at the worker's approval gate.
type ApproveOnGateData struct {
	SessionID      string
	CommandID      string
	AddToWhitelist bool
}

// DenyOnGateData rejects a suspended command at the worker's approval gate.
type DenyOnGateData struct {
	SessionID string
	CommandID string
	Reason    string
}

// CleanupSessionData is the payload for cleaning up all session data on a worker.
type CleanupSessionData struct {
	SessionID string
}

// SyncMountData is the payload for syncing a storage mount on a worker.
type SyncMountData struct {
	SessionID string
	MountPath string
	Direction string // "push" or "pull"
	Prefix    string
	Delete    bool
	DryRun    bool
}

// LaunchServiceData is the payload for launching a service on a worker.
type LaunchServiceData struct {
	ServiceID     string
	ImageRef      string
	VCPUs         int
	MemoryMB      int
	RootfsPath    string
	Command       string
	Args          []string
	Env           map[string]string
	Workdir       string
	Port          int
	BundleKey     string
	RestartPolicy string
	Mounts        []loka.VolumeMount
}

// StopServiceData is the payload for stopping a service on a worker.
type StopServiceData struct {
	ServiceID string
}
