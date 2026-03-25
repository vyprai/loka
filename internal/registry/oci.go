package registry

// OCI Image Manifest (OCI Image Manifest v1).
type OCIManifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

// Descriptor describes a content-addressable blob.
type Descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"` // sha256:abc123...
	Size      int64  `json:"size"`
}

// DockerManifest is a Docker Image Manifest v2 Schema 2.
type DockerManifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

// OCI / Docker media types.
const (
	MediaTypeOCIManifest       = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIConfig         = "application/vnd.oci.image.config.v1+json"
	MediaTypeOCILayer          = "application/vnd.oci.image.layer.v1.tar+gzip"
	MediaTypeDockerManifest    = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerConfig      = "application/vnd.docker.container.image.v1+json"
	MediaTypeDockerLayer       = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	MediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeOCIIndex          = "application/vnd.oci.image.index.v1+json"
)

// ManifestList is a Docker manifest list / OCI index (multi-arch).
type ManifestList struct {
	SchemaVersion int                `json:"schemaVersion"`
	MediaType     string             `json:"mediaType"`
	Manifests     []PlatformManifest `json:"manifests"`
}

// PlatformManifest is a single platform entry in a manifest list.
type PlatformManifest struct {
	MediaType string   `json:"mediaType"`
	Digest    string   `json:"digest"`
	Size      int64    `json:"size"`
	Platform  Platform `json:"platform"`
}

// Platform describes the OS and architecture of a manifest.
type Platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}
