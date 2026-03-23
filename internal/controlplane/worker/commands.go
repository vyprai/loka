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
