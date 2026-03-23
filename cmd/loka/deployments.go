package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Deployment is a saved LOKA deployment.
type Deployment struct {
	Name      string            `json:"name"`
	Provider  string            `json:"provider"`  // local, aws, gcp, azure, digitalocean, ovh
	Region    string            `json:"region"`
	Endpoint  string            `json:"endpoint"`  // Control plane URL.
	Token     string            `json:"token"`     // API key for this deployment.
	Workers   int               `json:"workers"`
	Status    string            `json:"status"`    // running, stopped, unknown
	CreatedAt time.Time         `json:"created_at"`
	Meta      map[string]string `json:"meta,omitempty"` // Provider-specific metadata.
}

// DeploymentStore manages the deployment state file.
type DeploymentStore struct {
	Active      string       `json:"active"` // Name of the active deployment.
	Deployments []Deployment `json:"deployments"`
}

func deploymentsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".loka", "deployments.json")
}

func loadDeployments() (*DeploymentStore, error) {
	path := deploymentsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &DeploymentStore{}, nil
		}
		return nil, err
	}
	var store DeploymentStore
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, err
	}
	return &store, nil
}

func saveDeployments(store *DeploymentStore) error {
	path := deploymentsPath()
	os.MkdirAll(filepath.Dir(path), 0o700)
	data, _ := json.MarshalIndent(store, "", "  ")
	return os.WriteFile(path, data, 0o600)
}

func (s *DeploymentStore) Add(d Deployment) {
	// Replace if same name exists.
	for i, existing := range s.Deployments {
		if existing.Name == d.Name {
			s.Deployments[i] = d
			return
		}
	}
	s.Deployments = append(s.Deployments, d)
}

func (s *DeploymentStore) Get(name string) *Deployment {
	for i := range s.Deployments {
		if s.Deployments[i].Name == name {
			return &s.Deployments[i]
		}
	}
	return nil
}

func (s *DeploymentStore) GetActive() *Deployment {
	if s.Active == "" && len(s.Deployments) > 0 {
		return &s.Deployments[0]
	}
	return s.Get(s.Active)
}

func (s *DeploymentStore) Remove(name string) bool {
	for i, d := range s.Deployments {
		if d.Name == name {
			s.Deployments = append(s.Deployments[:i], s.Deployments[i+1:]...)
			if s.Active == name {
				s.Active = ""
			}
			return true
		}
	}
	return false
}

func (s *DeploymentStore) SetActive(name string) error {
	if s.Get(name) == nil {
		return fmt.Errorf("server %q not found", name)
	}
	s.Active = name
	return nil
}

// activeEndpoint returns the endpoint of the active deployment,
// falling back to the --server flag.
func activeEndpoint() string {
	store, err := loadDeployments()
	if err != nil {
		return serverAddr
	}
	d := store.GetActive()
	if d != nil && d.Endpoint != "" {
		return d.Endpoint
	}
	return serverAddr
}
