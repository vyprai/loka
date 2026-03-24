package recipe

import (
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed builtin/*
var builtinFS embed.FS

// Recipe defines a project recipe configuration.
type Recipe struct {
	Name        string            `yaml:"name"`
	Version     string            `yaml:"version"`
	Description string            `yaml:"description"`
	Before      []string          `yaml:"before"`  // Check before these recipes
	Image       string            `yaml:"image"`   // Docker image for runtime
	Port        int               `yaml:"port"`
	Build       []string          `yaml:"build"`   // Build commands (run locally)
	Include     []string          `yaml:"include"` // Files to bundle after build
	Start       string            `yaml:"start"`   // Start command in VM
	Health      HealthConfig      `yaml:"health"`
	Env         map[string]string `yaml:"env"`

	// matchScript holds the JS match script content (not serialized).
	matchScript string
	// source indicates where the recipe was loaded from.
	source string
}

// HealthConfig defines health check parameters.
type HealthConfig struct {
	Path     string `yaml:"path"`
	Interval int    `yaml:"interval"`
	Timeout  int    `yaml:"timeout"`
	Retries  int    `yaml:"retries"`
}

// LoadFromDir reads a recipe.yaml from a directory on disk.
func LoadFromDir(dir string) (*Recipe, error) {
	yamlPath := filepath.Join(dir, "recipe.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("read recipe %s: %w", yamlPath, err)
	}

	var r Recipe
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse recipe %s: %w", yamlPath, err)
	}

	// Load match.js if present.
	matchPath := filepath.Join(dir, "match.js")
	if matchData, err := os.ReadFile(matchPath); err == nil {
		r.matchScript = string(matchData)
	}

	r.source = dir
	return &r, nil
}

// LoadBuiltin loads an embedded built-in recipe by name.
func LoadBuiltin(name string) (*Recipe, error) {
	dir := "builtin/" + name
	yamlPath := dir + "/recipe.yaml"

	data, err := builtinFS.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("load builtin recipe %q: %w", name, err)
	}

	var r Recipe
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse builtin recipe %q: %w", name, err)
	}

	// Load match.js if present.
	matchPath := dir + "/match.js"
	if matchData, err := builtinFS.ReadFile(matchPath); err == nil {
		r.matchScript = string(matchData)
	}

	r.source = "builtin:" + name
	return &r, nil
}

// ListAll returns all available recipes: built-in recipes first, then
// user-installed recipes from ~/.loka/recipes/.
func ListAll() ([]*Recipe, error) {
	var recipes []*Recipe

	// Load built-in recipes.
	entries, err := builtinFS.ReadDir("builtin")
	if err != nil {
		return nil, fmt.Errorf("read builtin recipes: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		r, err := LoadBuiltin(entry.Name())
		if err != nil {
			slog.Warn("skip builtin recipe", "name", entry.Name(), "error", err)
			continue
		}
		recipes = append(recipes, r)
	}

	// Load user recipes.
	userDir := UserRecipesDir()
	if entries, err := os.ReadDir(userDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(userDir, entry.Name())
			// Only load if recipe.yaml exists.
			if _, err := os.Stat(filepath.Join(dir, "recipe.yaml")); err != nil {
				continue
			}
			r, err := LoadFromDir(dir)
			if err != nil {
				slog.Warn("skip user recipe", "dir", dir, "error", err)
				continue
			}
			recipes = append(recipes, r)
		}
	}

	return recipes, nil
}

// UserRecipesDir returns the path to user-installed recipes.
func UserRecipesDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".loka", "recipes")
	}
	return filepath.Join(home, ".loka", "recipes")
}

// MatchScript returns the JS match script for this recipe.
func (r *Recipe) MatchScript() string {
	return r.matchScript
}

// Source returns where this recipe was loaded from.
func (r *Recipe) Source() string {
	return r.source
}

// builtinRecipeNames returns the names of all built-in recipe directories.
func builtinRecipeNames() ([]string, error) {
	entries, err := builtinFS.ReadDir("builtin")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			// Verify it has a recipe.yaml.
			if _, err := fs.Stat(builtinFS, "builtin/"+e.Name()+"/recipe.yaml"); err == nil {
				names = append(names, e.Name())
			}
		}
	}
	return names, nil
}

// String returns a human-readable representation of the recipe.
func (r *Recipe) String() string {
	var sb strings.Builder
	sb.WriteString(r.Name)
	if r.Version != "" {
		sb.WriteString(" v" + r.Version)
	}
	if r.Description != "" {
		sb.WriteString(" - " + r.Description)
	}
	return sb.String()
}
