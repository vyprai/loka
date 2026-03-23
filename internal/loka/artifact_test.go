package loka

import (
	"encoding/json"
	"testing"
	"time"
)

func TestArtifact_JSONRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	a := Artifact{
		ID:           "art-1",
		SessionID:    "sess-1",
		CheckpointID: "cp-1",
		Path:         "/workspace/output.csv",
		Size:         4096,
		Hash:         "sha256:abc123",
		Type:         "added",
		IsDir:        false,
		CreatedAt:    now,
	}

	data, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Artifact
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.ID != a.ID {
		t.Errorf("ID: got %q, want %q", got.ID, a.ID)
	}
	if got.SessionID != a.SessionID {
		t.Errorf("SessionID: got %q, want %q", got.SessionID, a.SessionID)
	}
	if got.CheckpointID != a.CheckpointID {
		t.Errorf("CheckpointID: got %q, want %q", got.CheckpointID, a.CheckpointID)
	}
	if got.Path != a.Path {
		t.Errorf("Path: got %q, want %q", got.Path, a.Path)
	}
	if got.Size != a.Size {
		t.Errorf("Size: got %d, want %d", got.Size, a.Size)
	}
	if got.Hash != a.Hash {
		t.Errorf("Hash: got %q, want %q", got.Hash, a.Hash)
	}
	if got.Type != a.Type {
		t.Errorf("Type: got %q, want %q", got.Type, a.Type)
	}
	if got.IsDir != a.IsDir {
		t.Errorf("IsDir: got %v, want %v", got.IsDir, a.IsDir)
	}

	// Verify JSON field names.
	var raw map[string]any
	json.Unmarshal(data, &raw)
	for _, key := range []string{"id", "session_id", "checkpoint_id", "path", "size", "hash", "type", "created_at"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing JSON key %q", key)
		}
	}
}

func TestArtifact_EmptyCheckpointIDOmitted(t *testing.T) {
	a := Artifact{
		ID:        "art-2",
		SessionID: "sess-2",
		Path:      "/workspace/file.txt",
		Size:      100,
		Hash:      "sha256:def456",
		Type:      "modified",
		CreatedAt: time.Now(),
	}

	data, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	json.Unmarshal(data, &raw)
	if _, ok := raw["checkpoint_id"]; ok {
		t.Error("checkpoint_id should be omitted when empty")
	}
}

func TestArtifact_IsDirTrue(t *testing.T) {
	a := Artifact{
		ID:        "art-3",
		SessionID: "sess-3",
		Path:      "/workspace/output/",
		Size:      0,
		Hash:      "",
		Type:      "added",
		IsDir:     true,
		CreatedAt: time.Now(),
	}

	data, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	json.Unmarshal(data, &raw)
	isDir, ok := raw["is_dir"]
	if !ok {
		t.Fatal("is_dir should be present when true")
	}
	if isDir != true {
		t.Errorf("is_dir: got %v, want true", isDir)
	}

	// Verify round-trip preserves IsDir.
	var got Artifact
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.IsDir {
		t.Error("IsDir should be true after round-trip")
	}
}

func TestArtifact_IsDirFalseOmitted(t *testing.T) {
	a := Artifact{
		ID:        "art-4",
		SessionID: "sess-4",
		Path:      "/workspace/file.txt",
		Type:      "added",
		IsDir:     false,
		CreatedAt: time.Now(),
	}

	data, _ := json.Marshal(a)
	var raw map[string]any
	json.Unmarshal(data, &raw)
	if _, ok := raw["is_dir"]; ok {
		t.Error("is_dir should be omitted when false")
	}
}
