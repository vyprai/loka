package loka

// StorageMount defines an object storage bucket mounted into a session's VM.
type StorageMount struct {
	// Name is a human-readable identifier for this mount.
	Name string `json:"name,omitempty"`

	// Provider is the storage backend: "s3", "gcs", "azure-blob", "local".
	Provider string `json:"provider"`

	// Bucket is the bucket or container name.
	Bucket string `json:"bucket"`

	// Prefix limits the mount to a key prefix within the bucket (optional).
	// e.g. "datasets/2024/" → only keys under that prefix are visible.
	Prefix string `json:"prefix,omitempty"`

	// MountPath is where the storage appears inside the VM's filesystem.
	// e.g. "/data", "/mnt/s3"
	MountPath string `json:"mount_path"`

	// ReadOnly makes the mount read-only inside the VM.
	ReadOnly bool `json:"read_only,omitempty"`

	// Credentials for accessing the storage.
	// These are passed to the in-VM mount agent, not stored in the database.
	Credentials *StorageCredentials `json:"credentials,omitempty"`

	// Region for the storage bucket (optional, used for S3/GCS).
	Region string `json:"region,omitempty"`

	// Endpoint for S3-compatible storage (MinIO, R2, etc). Optional.
	Endpoint string `json:"endpoint,omitempty"`
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
