package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeploymentStoreAdd(t *testing.T) {
	store := &DeploymentStore{}

	d := Deployment{
		Name:      "prod",
		Provider:  "aws",
		Region:    "us-east-1",
		Endpoint:  "https://prod.example.com",
		Token:     "tok-1",
		Workers:   3,
		Status:    "running",
		CreatedAt: time.Now(),
	}
	store.Add(d)

	if len(store.Deployments) != 1 {
		t.Fatalf("expected 1 deployment, got %d", len(store.Deployments))
	}
	if store.Deployments[0].Name != "prod" {
		t.Errorf("expected name %q, got %q", "prod", store.Deployments[0].Name)
	}
	if store.Deployments[0].Provider != "aws" {
		t.Errorf("expected provider %q, got %q", "aws", store.Deployments[0].Provider)
	}
}

func TestDeploymentStoreAddReplacesExisting(t *testing.T) {
	store := &DeploymentStore{}

	store.Add(Deployment{Name: "dev", Endpoint: "http://old.local", Workers: 1})
	store.Add(Deployment{Name: "dev", Endpoint: "http://new.local", Workers: 5})

	if len(store.Deployments) != 1 {
		t.Fatalf("expected 1 deployment after replace, got %d", len(store.Deployments))
	}
	if store.Deployments[0].Endpoint != "http://new.local" {
		t.Errorf("expected endpoint %q, got %q", "http://new.local", store.Deployments[0].Endpoint)
	}
	if store.Deployments[0].Workers != 5 {
		t.Errorf("expected workers 5, got %d", store.Deployments[0].Workers)
	}
}

func TestDeploymentStoreGet(t *testing.T) {
	store := &DeploymentStore{}
	store.Add(Deployment{Name: "alpha", Endpoint: "https://alpha.test"})
	store.Add(Deployment{Name: "beta", Endpoint: "https://beta.test"})

	got := store.Get("alpha")
	if got == nil {
		t.Fatal("expected to find deployment 'alpha'")
	}
	if got.Endpoint != "https://alpha.test" {
		t.Errorf("expected endpoint %q, got %q", "https://alpha.test", got.Endpoint)
	}

	got = store.Get("beta")
	if got == nil {
		t.Fatal("expected to find deployment 'beta'")
	}
	if got.Endpoint != "https://beta.test" {
		t.Errorf("expected endpoint %q, got %q", "https://beta.test", got.Endpoint)
	}
}

func TestDeploymentStoreGetNotFound(t *testing.T) {
	store := &DeploymentStore{}
	store.Add(Deployment{Name: "existing"})

	got := store.Get("nonexistent")
	if got != nil {
		t.Errorf("expected nil for nonexistent deployment, got %+v", got)
	}
}

func TestDeploymentStoreGetActive(t *testing.T) {
	store := &DeploymentStore{}
	store.Add(Deployment{Name: "first", Endpoint: "https://first.test"})
	store.Add(Deployment{Name: "second", Endpoint: "https://second.test"})
	store.Active = "second"

	active := store.GetActive()
	if active == nil {
		t.Fatal("expected active deployment")
	}
	if active.Name != "second" {
		t.Errorf("expected active %q, got %q", "second", active.Name)
	}
}

func TestDeploymentStoreGetActiveFallsBackToFirst(t *testing.T) {
	store := &DeploymentStore{}
	store.Add(Deployment{Name: "only-one", Endpoint: "https://only.test"})

	// Active is empty, should fall back to the first deployment.
	active := store.GetActive()
	if active == nil {
		t.Fatal("expected fallback to first deployment")
	}
	if active.Name != "only-one" {
		t.Errorf("expected %q, got %q", "only-one", active.Name)
	}
}

func TestDeploymentStoreGetActiveEmpty(t *testing.T) {
	store := &DeploymentStore{}

	active := store.GetActive()
	if active != nil {
		t.Errorf("expected nil active from empty store, got %+v", active)
	}
}

func TestDeploymentStoreRemove(t *testing.T) {
	store := &DeploymentStore{}
	store.Add(Deployment{Name: "remove-me"})
	store.Add(Deployment{Name: "keep-me"})

	ok := store.Remove("remove-me")
	if !ok {
		t.Error("expected Remove to return true")
	}
	if len(store.Deployments) != 1 {
		t.Fatalf("expected 1 deployment, got %d", len(store.Deployments))
	}
	if store.Deployments[0].Name != "keep-me" {
		t.Errorf("expected remaining deployment %q, got %q", "keep-me", store.Deployments[0].Name)
	}
}

func TestDeploymentStoreRemoveNotFound(t *testing.T) {
	store := &DeploymentStore{}
	store.Add(Deployment{Name: "exists"})

	ok := store.Remove("does-not-exist")
	if ok {
		t.Error("expected Remove to return false for nonexistent deployment")
	}
	if len(store.Deployments) != 1 {
		t.Errorf("expected 1 deployment unchanged, got %d", len(store.Deployments))
	}
}

func TestDeploymentStoreRemoveClearsActive(t *testing.T) {
	store := &DeploymentStore{}
	store.Add(Deployment{Name: "active-one"})
	store.Active = "active-one"

	store.Remove("active-one")
	if store.Active != "" {
		t.Errorf("expected Active to be cleared, got %q", store.Active)
	}
}

func TestDeploymentStoreSetActive(t *testing.T) {
	store := &DeploymentStore{}
	store.Add(Deployment{Name: "a"})
	store.Add(Deployment{Name: "b"})

	if err := store.SetActive("b"); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if store.Active != "b" {
		t.Errorf("expected Active %q, got %q", "b", store.Active)
	}

	if err := store.SetActive("a"); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if store.Active != "a" {
		t.Errorf("expected Active %q, got %q", "a", store.Active)
	}
}

func TestDeploymentStoreSetActiveInvalidName(t *testing.T) {
	store := &DeploymentStore{}
	store.Add(Deployment{Name: "valid"})

	err := store.SetActive("invalid")
	if err == nil {
		t.Fatal("expected error for invalid deployment name")
	}
}

func TestDeploymentStoreLoadSaveRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "deployments.json")

	// Build a store with data.
	original := &DeploymentStore{
		Active: "staging",
		Deployments: []Deployment{
			{
				Name:      "staging",
				Provider:  "gcp",
				Region:    "us-central1",
				Endpoint:  "https://staging.loka.dev",
				Token:     "stg-token-123",
				Workers:   2,
				Status:    "running",
				CreatedAt: time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC),
				Meta:      map[string]string{"project": "loka-staging"},
			},
			{
				Name:      "production",
				Provider:  "aws",
				Region:    "eu-west-1",
				Endpoint:  "https://prod.loka.dev",
				Token:     "prod-token-456",
				Workers:   10,
				Status:    "running",
				CreatedAt: time.Date(2025, 3, 1, 8, 0, 0, 0, time.UTC),
			},
		},
	}

	// Write to file.
	data, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Read back.
	readData, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var loaded DeploymentStore
	if err := json.Unmarshal(readData, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Verify.
	if loaded.Active != original.Active {
		t.Errorf("Active: expected %q, got %q", original.Active, loaded.Active)
	}
	if len(loaded.Deployments) != len(original.Deployments) {
		t.Fatalf("expected %d deployments, got %d", len(original.Deployments), len(loaded.Deployments))
	}

	for i, orig := range original.Deployments {
		got := loaded.Deployments[i]
		if got.Name != orig.Name {
			t.Errorf("[%d] name: expected %q, got %q", i, orig.Name, got.Name)
		}
		if got.Provider != orig.Provider {
			t.Errorf("[%d] provider: expected %q, got %q", i, orig.Provider, got.Provider)
		}
		if got.Region != orig.Region {
			t.Errorf("[%d] region: expected %q, got %q", i, orig.Region, got.Region)
		}
		if got.Endpoint != orig.Endpoint {
			t.Errorf("[%d] endpoint: expected %q, got %q", i, orig.Endpoint, got.Endpoint)
		}
		if got.Token != orig.Token {
			t.Errorf("[%d] token: expected %q, got %q", i, orig.Token, got.Token)
		}
		if got.Workers != orig.Workers {
			t.Errorf("[%d] workers: expected %d, got %d", i, orig.Workers, got.Workers)
		}
		if got.Status != orig.Status {
			t.Errorf("[%d] status: expected %q, got %q", i, orig.Status, got.Status)
		}
	}

	// Verify meta is preserved.
	if loaded.Deployments[0].Meta["project"] != "loka-staging" {
		t.Errorf("meta lost: expected 'loka-staging', got %q", loaded.Deployments[0].Meta["project"])
	}
}

func TestDeploymentStoreLoadSaveWithMethods(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "store.json")

	// Create and populate store.
	store := &DeploymentStore{}
	store.Add(Deployment{Name: "local", Provider: "local", Endpoint: "http://localhost:6840"})
	store.Add(Deployment{Name: "cloud", Provider: "aws", Endpoint: "https://cloud.loka.dev"})
	store.SetActive("cloud")

	// Save.
	data, _ := json.MarshalIndent(store, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Load.
	readData, _ := os.ReadFile(path)
	var loaded DeploymentStore
	json.Unmarshal(readData, &loaded)

	// Verify active works correctly after round-trip.
	active := loaded.GetActive()
	if active == nil {
		t.Fatal("expected active deployment after load")
	}
	if active.Name != "cloud" {
		t.Errorf("expected active %q, got %q", "cloud", active.Name)
	}

	// Verify Get still works.
	local := loaded.Get("local")
	if local == nil {
		t.Fatal("expected to find 'local' deployment")
	}
	if local.Endpoint != "http://localhost:6840" {
		t.Errorf("expected endpoint %q, got %q", "http://localhost:6840", local.Endpoint)
	}
}
