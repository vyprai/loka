package loka

import "time"

// VolumeType represents the storage mode for a volume.
type VolumeType string

const (
	// VolumeTypeBlock is a folder on the worker host, mounted via virtiofs.
	// Replicated across workers (2 copies default). Can be shared with locking.
	VolumeTypeBlock VolumeType = "block"
	// VolumeTypeObject is backed by object storage (S3/GCS/Azure).
	// Can be Loka-managed (default bucket) or direct-connection (user's bucket).
	VolumeTypeObject VolumeType = "object"
)

// VolumeStatus represents the replication health of a volume.
type VolumeStatus string

const (
	VolumeStatusHealthy  VolumeStatus = "healthy"  // All replicas present.
	VolumeStatusDegraded VolumeStatus = "degraded" // Fewer replicas than desired.
	VolumeStatusSyncing  VolumeStatus = "syncing"  // Replica catch-up in progress.
	VolumeStatusError    VolumeStatus = "error"     // Data loss (all copies gone).
)

// Volume is the unified mount type for both sessions and services.
// Providers:
//   - "local":   Host directory shared via virtiofs (same host only).
//   - "volume":  Named persistent volume (host directory managed by worker).
//   - "store":   Shared storage (cross-worker sync, lockable via control plane).
//   - "github":  Git repository checkout (cached by commit SHA, readonly).
//   - "s3":      S3 object storage bucket.
//   - "gcs":     Google Cloud Storage bucket.
//   - "azure":   Azure Blob Storage container.
//
// All are served to guest VMs via virtiofs for transparent POSIX access.
type Volume struct {
	Path        string `json:"path"`
	Type        string `json:"type,omitempty"`         // "block" or "object" (default: auto-detect)
	Provider    string `json:"provider"`               // "local", "volume", "store", "github", "git", "s3", "gcs", "azure"
	Name        string `json:"name,omitempty"`          // Volume/store name (provider=volume/store)
	Bucket      string `json:"bucket,omitempty"`
	Prefix      string `json:"prefix,omitempty"`
	Region      string `json:"region,omitempty"`
	Credentials string `json:"credentials,omitempty"`   // ${secret.name}
	Access      string `json:"access,omitempty"`        // "readonly" or "readwrite" (default)

	// Host directory (local sharing, same host only).
	HostPath string `json:"host_path,omitempty"`       // Direct host dir path.

	// Git repository (provider="github" or "git").
	GitRepo string `json:"git_repo,omitempty"`         // "owner/repo" or full HTTPS URL.
	GitRef  string `json:"git_ref,omitempty"`          // Branch, tag, or commit SHA (default: HEAD).
}

// VolumeRecord is a persistent record for a named volume tracked in the store.
// CP stores metadata only — actual data lives on workers (block) or objstore (object).
type VolumeRecord struct {
	Name             string       `json:"name"`
	Type             VolumeType   `json:"type"`                // "block" or "object"
	Status           VolumeStatus `json:"status"`
	Provider         string       `json:"provider"`            // "volume" (block/loka-managed), "s3"/"gcs"/"azure" (direct)
	SizeBytes        int64        `json:"size_bytes"`          // Current usage reported by primary worker.
	MaxSizeBytes     int64        `json:"max_size_bytes"`      // Optional max size (0 = unlimited).
	PrimaryWorkerID  string       `json:"primary_worker_id"`   // Worker holding the primary copy (block/loka-managed).
	ReplicaWorkerIDs []string     `json:"replica_worker_ids"`  // Workers holding replicas.
	DesiredReplicas  int          `json:"desired_replicas"`    // Target replica count (default 2).
	MountCount       int          `json:"mount_count"`         // Number of VMs currently mounting this volume.
	// Direct-connection object volume fields.
	Bucket      string `json:"bucket,omitempty"`
	Prefix      string `json:"prefix,omitempty"`
	Region      string `json:"region,omitempty"`
	Credentials string `json:"credentials,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// IsDirectObject returns true if this is a direct-connection object volume
// where the user provides their own bucket/credentials.
func (v VolumeRecord) IsDirectObject() bool {
	return v.Type == VolumeTypeObject && v.Bucket != ""
}

// IsLokaManaged returns true if this volume's data is managed by Loka
// (block volumes, or object volumes without a direct bucket connection).
func (v VolumeRecord) IsLokaManaged() bool {
	return v.Type == VolumeTypeBlock || (v.Type == VolumeTypeObject && v.Bucket == "")
}

// IsReadOnly returns true if the volume is read-only.
func (v Volume) IsReadOnly() bool {
	return v.Access == "readonly"
}

// EffectiveMode returns the mount mode for the VMM.
// All volume types now use virtiofs (data always on local worker disk).
func (v Volume) EffectiveMode() string {
	switch v.Provider {
	case "github", "git", "store", "local", "volume":
		return "virtiofs"
	}
	if v.HostPath != "" || v.Type == "block" || v.Type == "object" || v.Type == "network" || v.Type == "store" {
		return "virtiofs"
	}
	if v.IsReadOnly() {
		return "block"
	}
	return "fuse"
}

// EffectiveType returns the volume type, auto-detecting from provider if not set.
func (v Volume) EffectiveType() string {
	if v.Type != "" {
		return v.Type
	}
	switch v.Provider {
	case "volume", "local", "store":
		return "block"
	case "s3", "gcs", "azure":
		return "object"
	case "github", "git":
		return "network" // read-only git mounts, unchanged
	}
	if v.HostPath != "" {
		return "block"
	}
	if v.Bucket != "" {
		return "object"
	}
	return "block"
}
