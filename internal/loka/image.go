package loka

import "time"

// ImageLayer represents a single Docker image layer stored as an ext4 file.
// Layers are deduplicated by digest across images — if two images share a
// base layer (e.g. ubuntu:22.04), the layer is stored and uploaded only once.
type ImageLayer struct {
	Digest string `json:"digest"`  // Content-addressable ID: "sha256:abc123..."
	Size   int64  `json:"size"`    // Uncompressed layer tar size in bytes.
	ObjKey string `json:"obj_key"` // Object store key: "sha256:abc123.../layer.ext4"
}

// Image is a base image backed by a Docker/OCI container image.
// It is pulled from a registry, saved with `docker save`, and its layers
// are extracted, deduplicated, and combined into a layer-pack ext4 file
// used as the read-only base for Firecracker microVMs.
//
// Flow:
//   1. User specifies a Docker image: "ubuntu:22.04", "python:3.12-slim"
//   2. LOKA pulls the image via Docker
//   3. `docker save` exports the image as a tar with individual layers
//   4. Each layer is extracted, hashed, and uploaded (deduplicated by digest)
//   5. All layers are combined into a layer-pack ext4 (numbered /0/, /1/, ...)
//   6. The loka-supervisor binary is injected into the top layer
//   7. Optionally boots in Firecracker and creates a "warm snapshot"
//   8. Future sessions restore from this snapshot (~instant startup)
//
// The layer-pack is READ-ONLY. All session changes go to an overlay layer.
// Snapshots capture the overlay diff from the base image.
type Image struct {
	ID          string    // Unique image ID (hash-based).
	Reference   string    // Docker reference: "ubuntu:22.04", "ghcr.io/org/image:tag"
	Digest      string    // Image digest: "sha256:abc123..."
	RootfsPath  string    // Object store key for the layer-pack ext4 (backward compat).
	Layers       []ImageLayer `json:"layers"`         // Ordered bottom-to-top.
	LayerPackKey string       `json:"layer_pack_key"` // Object store key for combined layer-pack ext4.
	SnapshotMem string    // Path to warm snapshot memory file (optional).
	SnapshotVMState string // Path to warm snapshot VM state file (optional).
	SizeMB      int64     // Layer-pack size in MB.
	Status      ImageStatus
	CreatedAt   time.Time
}

// ImageStatus tracks the state of an image.
type ImageStatus string

const (
	ImageStatusPulling    ImageStatus = "pulling"    // Downloading from registry.
	ImageStatusConverting ImageStatus = "converting"  // Converting to ext4.
	ImageStatusWarming    ImageStatus = "warming"     // Booting to create warm snapshot.
	ImageStatusReady      ImageStatus = "ready"       // Ready to use.
	ImageStatusFailed     ImageStatus = "failed"
)

// Snapshot is the diff between a session's current state and its base image.
// It captures everything that changed after the base image booted:
// installed packages, config changes, workspace files, etc.
//
// Architecture:
//   Base Image (rootfs.ext4, RO)
//   + Overlay Layer (snapshot diff, RW during session)
//   = Complete VM filesystem
//
// When creating a snapshot:
//   1. Pause the VM
//   2. Capture the overlay diff (everything written since boot)
//   3. Optionally capture VM memory state (for instant restore)
//   4. Upload overlay + memory to object store
//
// When restoring from a snapshot:
//   1. Start from the same base image
//   2. Apply the overlay diff on top
//   3. Optionally restore VM memory state
//   4. Resume — VM is in the exact same state
type Snapshot struct {
	ID          string
	Name        string
	ImageID     string    // Base image this snapshot is relative to.
	ImageRef    string    // Docker reference of the base image.
	OverlayPath string    // Object store path to overlay diff.
	MemPath     string    // Object store path to VM memory (empty for light snapshots).
	VMStatePath string    // Object store path to VM state.
	SizeMB      int64     // Overlay size.
	Status      SnapshotStatus
	Labels      map[string]string
	CreatedAt   time.Time
}

// SnapshotStatus tracks the state of a snapshot.
type SnapshotStatus string

const (
	SnapshotStatusCreating SnapshotStatus = "creating"
	SnapshotStatusReady    SnapshotStatus = "ready"
	SnapshotStatusFailed   SnapshotStatus = "failed"
)
