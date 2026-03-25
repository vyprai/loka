package loka

import (
	"encoding/json"
	"testing"
)

func TestPortMapping_JSON(t *testing.T) {
	pm := PortMapping{
		LocalPort:  8080,
		RemotePort: 5000,
		Protocol:   "tcp",
	}
	data, err := json.Marshal(pm)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got PortMapping
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != pm {
		t.Errorf("roundtrip: got %+v, want %+v", got, pm)
	}

	// Verify JSON field names.
	var raw map[string]any
	json.Unmarshal(data, &raw)
	if _, ok := raw["local_port"]; !ok {
		t.Error("missing JSON key 'local_port'")
	}
	if _, ok := raw["remote_port"]; !ok {
		t.Error("missing JSON key 'remote_port'")
	}
}

func TestPortMapping_OmitEmptyProtocol(t *testing.T) {
	pm := PortMapping{LocalPort: 8080, RemotePort: 5000}
	data, _ := json.Marshal(pm)
	var raw map[string]any
	json.Unmarshal(data, &raw)
	if _, ok := raw["protocol"]; ok {
		t.Error("protocol should be omitted when empty")
	}
}

func TestVolume_JSON(t *testing.T) {
	v := Volume{
		Path:        "/data",
		Provider:    "s3",
		Bucket:      "my-bucket",
		Prefix:      "datasets/",
		Region:      "us-east-1",
		Credentials: "${secret.aws}",
		Access:      "readonly",
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Volume
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Provider != v.Provider {
		t.Errorf("Provider: got %q, want %q", got.Provider, v.Provider)
	}
	if got.Bucket != v.Bucket {
		t.Errorf("Bucket: got %q, want %q", got.Bucket, v.Bucket)
	}
	if got.Access != v.Access {
		t.Errorf("Access: got %q, want %q", got.Access, v.Access)
	}
	if got.Credentials != "${secret.aws}" {
		t.Errorf("Credentials: got %q, want %q", got.Credentials, "${secret.aws}")
	}
}

func TestVolume_OmitEmpty(t *testing.T) {
	v := Volume{Path: "/m", Provider: "s3"}
	data, _ := json.Marshal(v)
	var raw map[string]any
	json.Unmarshal(data, &raw)
	for _, key := range []string{"name", "bucket", "prefix", "credentials", "region", "access"} {
		if _, ok := raw[key]; ok {
			t.Errorf("key %q should be omitted when empty/zero", key)
		}
	}
}

func TestVolume_IsReadOnly(t *testing.T) {
	v := Volume{Access: "readonly"}
	if !v.IsReadOnly() {
		t.Error("expected IsReadOnly to return true")
	}
	v.Access = "readwrite"
	if v.IsReadOnly() {
		t.Error("expected IsReadOnly to return false")
	}
}

func TestVolume_EffectiveMode(t *testing.T) {
	v := Volume{Access: "readonly"}
	if v.EffectiveMode() != "block" {
		t.Errorf("expected block, got %s", v.EffectiveMode())
	}
	v.Access = "readwrite"
	if v.EffectiveMode() != "fuse" {
		t.Errorf("expected fuse, got %s", v.EffectiveMode())
	}
}

func TestSyncDirection_Constants(t *testing.T) {
	if SyncPush != "push" {
		t.Errorf("SyncPush = %q, want %q", SyncPush, "push")
	}
	if SyncPull != "pull" {
		t.Errorf("SyncPull = %q, want %q", SyncPull, "pull")
	}
}

func TestSyncRequest_JSON(t *testing.T) {
	sr := SyncRequest{
		MountPath: "/data",
		Direction: SyncPush,
		Prefix:    "results/",
		Delete:    true,
		DryRun:    false,
	}
	data, err := json.Marshal(sr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got SyncRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.MountPath != sr.MountPath {
		t.Errorf("MountPath: got %q, want %q", got.MountPath, sr.MountPath)
	}
	if got.Direction != sr.Direction {
		t.Errorf("Direction: got %q, want %q", got.Direction, sr.Direction)
	}
	if got.Delete != sr.Delete {
		t.Errorf("Delete: got %v, want %v", got.Delete, sr.Delete)
	}
}

func TestSyncResult_JSON(t *testing.T) {
	sr := SyncResult{
		MountPath:        "/data",
		Direction:        "push",
		FilesAdded:       5,
		FilesUpdated:     2,
		FilesDeleted:     1,
		BytesTransferred: 1024000,
		Files:            []string{"a.txt", "b.txt"},
	}
	data, err := json.Marshal(sr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got SyncResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.FilesAdded != 5 {
		t.Errorf("FilesAdded: got %d, want 5", got.FilesAdded)
	}
	if got.BytesTransferred != 1024000 {
		t.Errorf("BytesTransferred: got %d, want 1024000", got.BytesTransferred)
	}
	if len(got.Files) != 2 {
		t.Errorf("Files length: got %d, want 2", len(got.Files))
	}
}

func TestSyncResult_OmitEmpty(t *testing.T) {
	sr := SyncResult{MountPath: "/data", Direction: "push"}
	data, _ := json.Marshal(sr)
	var raw map[string]any
	json.Unmarshal(data, &raw)
	if _, ok := raw["files"]; ok {
		t.Error("files should be omitted when nil")
	}
	if _, ok := raw["error"]; ok {
		t.Error("error should be omitted when empty")
	}
}
