package loka

import "time"

// Volume is the unified mount type for both sessions and services.
// It replaces both the old StorageMount (session) and VolumeMount (service) types.
type Volume struct {
	Path        string `json:"path"`
	Provider    string `json:"provider"`              // "s3", "gcs", "azure", "volume"
	Name        string `json:"name,omitempty"`         // Named volume (provider=volume)
	Bucket      string `json:"bucket,omitempty"`
	Prefix      string `json:"prefix,omitempty"`
	Region      string `json:"region,omitempty"`
	Credentials string `json:"credentials,omitempty"`  // ${secret.name}
	Access      string `json:"access,omitempty"`       // "readonly" or "readwrite" (default)
}

// VolumeRecord is a persistent record for a named volume tracked in the store.
// Multiple sessions/services can mount the same named volume.
type VolumeRecord struct {
	Name       string    `json:"name"`
	Provider   string    `json:"provider"`            // "volume" (objstore-backed)
	MountCount int       `json:"mount_count"`         // Number of VMs currently mounting this volume.
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// IsReadOnly returns true if the volume is read-only.
func (v Volume) IsReadOnly() bool {
	return v.Access == "readonly"
}

// EffectiveMode returns the mount mode: "block" for readonly, "fuse" for readwrite.
func (v Volume) EffectiveMode() string {
	if v.IsReadOnly() {
		return "block"
	}
	return "fuse"
}
