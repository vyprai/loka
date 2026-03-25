package registry

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// RegistryConfig represents a configured container registry.
type RegistryConfig struct {
	Name        string `yaml:"name"`
	URL         string `yaml:"url"`
	Token       string `yaml:"token,omitempty"`
	Credentials string `yaml:"credentials,omitempty"` // ${secret.name} or user:pass
	Default     bool   `yaml:"default,omitempty"`
	Builtin     bool   `yaml:"builtin,omitempty"`
}

type registriesFile struct {
	Registries []RegistryConfig `yaml:"registries"`
}

func registriesPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".loka", "registries.yaml")
}

// LoadRegistries loads registry configurations from ~/.loka/registries.yaml.
// If the file does not exist, returns a default list with Docker Hub.
func LoadRegistries() ([]RegistryConfig, error) {
	path := registriesPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultRegistries(), nil
		}
		return nil, fmt.Errorf("read registries file: %w", err)
	}

	var f registriesFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse registries file: %w", err)
	}

	// Ensure Docker Hub is always present.
	hasDockerHub := false
	for _, r := range f.Registries {
		if r.Name == "dockerhub" {
			hasDockerHub = true
			break
		}
	}
	if !hasDockerHub {
		f.Registries = append(defaultRegistries(), f.Registries...)
	}

	return f.Registries, nil
}

// SaveRegistries saves registry configurations to ~/.loka/registries.yaml.
func SaveRegistries(configs []RegistryConfig) error {
	path := registriesPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create registries directory: %w", err)
	}

	f := registriesFile{Registries: configs}
	data, err := yaml.Marshal(&f)
	if err != nil {
		return fmt.Errorf("marshal registries: %w", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write registries file: %w", err)
	}
	return nil
}

func defaultRegistries() []RegistryConfig {
	return []RegistryConfig{
		{
			Name:    "dockerhub",
			URL:     "https://registry-1.docker.io",
			Default: true,
			Builtin: true,
		},
	}
}
