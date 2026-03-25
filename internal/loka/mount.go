package loka

// DomainRouteType distinguishes session routes from service routes.
type DomainRouteType string

const (
	// DomainRouteSession routes traffic to a session VM.
	DomainRouteSession DomainRouteType = "session"
	// DomainRouteService routes traffic to a deployed service (supports cold-start wake).
	DomainRouteService DomainRouteType = "service"
)

// DomainRoute maps a subdomain to a session or service port, enabling public
// HTTP access via the control plane's reverse proxy.
// e.g. "my-app.loka.example.com" → session abc123, port 5000
type DomainRoute struct {
	ID         string          `json:"id,omitempty"`
	Subdomain  string          `json:"subdomain"`            // e.g. "my-app" → my-app.{base_domain}
	SessionID  string          `json:"session_id,omitempty"`
	ServiceID  string          `json:"service_id,omitempty"` // For service routes (cold-start wake).
	RemotePort int             `json:"remote_port"`          // Port inside the VM
	Type       DomainRouteType `json:"type,omitempty"`       // "session" or "service"
	CreatedAt  string          `json:"created_at,omitempty"`
}

// PortMapping maps a local port to a port inside the session VM.
type PortMapping struct {
	LocalPort  int    `json:"local_port"`            // Port on user's machine (0 = auto-assign).
	RemotePort int    `json:"remote_port"`           // Port inside the VM.
	Protocol   string `json:"protocol,omitempty"`    // "tcp" (default) or "udp".
}

// SyncDirection controls which way data flows during a sync.
type SyncDirection string

const (
	// SyncPush uploads changed files from the VM mount path back to the bucket.
	SyncPush SyncDirection = "push"
	// SyncPull downloads the latest files from the bucket into the VM mount path.
	SyncPull SyncDirection = "pull"
)

// SyncRequest describes a sync operation on a session's storage mount.
type SyncRequest struct {
	// MountPath identifies which mount to sync (matches Volume.Path).
	MountPath string `json:"mount_path"`
	// Direction: "push" (VM → bucket) or "pull" (bucket → VM).
	Direction SyncDirection `json:"direction"`
	// Prefix limits the sync to a sub-path within the mount (optional).
	// e.g. "results/" syncs only /data/results/* if mount_path is /data.
	Prefix string `json:"prefix,omitempty"`
	// Delete removes files in the destination that don't exist in the source.
	Delete bool `json:"delete,omitempty"`
	// DryRun lists what would be synced without actually doing it.
	DryRun bool `json:"dry_run,omitempty"`
}

// SyncResult describes the outcome of a sync operation.
type SyncResult struct {
	MountPath    string   `json:"mount_path"`
	Direction    string   `json:"direction"`
	FilesAdded   int      `json:"files_added"`
	FilesUpdated int      `json:"files_updated"`
	FilesDeleted int      `json:"files_deleted"`
	BytesTransferred int64 `json:"bytes_transferred"`
	Files        []string `json:"files,omitempty"` // List of affected files (populated in dry_run).
	Error        string   `json:"error,omitempty"`
}

// StorageCredentials holds access keys for object storage.
// Only one set of credentials should be provided based on the provider.
type StorageCredentials struct {
	// S3 / S3-compatible
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey  string `json:"secret_access_key,omitempty"`
	SessionToken    string `json:"session_token,omitempty"`

	// GCS
	ServiceAccountJSON string `json:"service_account_json,omitempty"`

	// Azure Blob
	AccountName string `json:"account_name,omitempty"`
	AccountKey  string `json:"account_key,omitempty"`
	SASToken    string `json:"sas_token,omitempty"`
}
