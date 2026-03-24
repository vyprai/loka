package recipe

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/dop251/goja"
)

// RecipeContext provides file operations and recipe mutation methods
// that are exposed to JS match/hook scripts via goja.
type RecipeContext struct {
	projectDir string
	recipe     *Recipe
	vm         *goja.Runtime
}

// NewRecipeContext creates a new JS execution context for recipe scripts.
// A new goja runtime is created per invocation. This is acceptable since
// recipe matching only runs at deploy time, not on the hot path. Pooling
// would add complexity for minimal gain.
func NewRecipeContext(projectDir string, r *Recipe) *RecipeContext {
	return &RecipeContext{
		projectDir: projectDir,
		recipe:     r,
	}
}

// validatePath ensures the resolved path stays within the project directory.
func (c *RecipeContext) validatePath(path string) error {
	abs := filepath.Join(c.projectDir, filepath.Clean(path))
	if !strings.HasPrefix(abs, c.projectDir) {
		return fmt.Errorf("path %q escapes project directory", path)
	}
	return nil
}

// FileExists checks whether a file or directory exists relative to the project dir.
func (c *RecipeContext) FileExists(path string) bool {
	if err := c.validatePath(path); err != nil {
		return false
	}
	full := filepath.Join(c.projectDir, path)
	_, err := os.Stat(full)
	return err == nil
}

// ReadFile reads a file relative to the project dir and returns its contents.
// Returns an empty string if the file cannot be read.
func (c *RecipeContext) ReadFile(path string) string {
	if err := c.validatePath(path); err != nil {
		slog.Debug("recipe context ReadFile path rejected", "path", path, "error", err)
		return ""
	}
	full := filepath.Join(c.projectDir, path)
	data, err := os.ReadFile(full)
	if err != nil {
		slog.Debug("recipe context ReadFile failed", "path", full, "error", err)
		return ""
	}
	return string(data)
}

// ReadJSON reads a JSON file and returns it as a map.
// Returns nil if the file cannot be read or parsed.
func (c *RecipeContext) ReadJSON(path string) map[string]interface{} {
	if err := c.validatePath(path); err != nil {
		slog.Debug("recipe context ReadJSON path rejected", "path", path, "error", err)
		return nil
	}
	full := filepath.Join(c.projectDir, path)
	data, err := os.ReadFile(full)
	if err != nil {
		slog.Debug("recipe context ReadJSON failed", "path", full, "error", err)
		return nil
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		slog.Debug("recipe context ReadJSON parse failed", "path", full, "error", err)
		return nil
	}
	return result
}

// ListFiles returns files matching a glob pattern relative to the project dir.
func (c *RecipeContext) ListFiles(pattern string) []string {
	if err := c.validatePath(pattern); err != nil {
		slog.Debug("recipe context ListFiles path rejected", "pattern", pattern, "error", err)
		return []string{}
	}
	full := filepath.Join(c.projectDir, pattern)
	matches, err := filepath.Glob(full)
	if err != nil {
		slog.Debug("recipe context ListFiles failed", "pattern", full, "error", err)
		return []string{}
	}
	// Return paths relative to project dir.
	result := make([]string, 0, len(matches))
	for _, m := range matches {
		rel, err := filepath.Rel(c.projectDir, m)
		if err != nil {
			rel = m
		}
		result = append(result, rel)
	}
	return result
}

// SetPort overrides the recipe port.
func (c *RecipeContext) SetPort(port int) {
	c.recipe.Port = port
}

// SetEnv sets an environment variable in the recipe.
func (c *RecipeContext) SetEnv(key, value string) {
	if c.recipe.Env == nil {
		c.recipe.Env = make(map[string]string)
	}
	c.recipe.Env[key] = value
}

// SetImage overrides the recipe Docker image.
func (c *RecipeContext) SetImage(image string) {
	c.recipe.Image = image
}

// SetBuildCommand replaces a build command at the given index.
func (c *RecipeContext) SetBuildCommand(idx int, cmd string) {
	if idx >= 0 && idx < len(c.recipe.Build) {
		c.recipe.Build[idx] = cmd
	}
}

// SetStartCommand overrides the recipe start command.
func (c *RecipeContext) SetStartCommand(cmd string) {
	c.recipe.Start = cmd
}

// RunScript executes a JS script in the goja runtime with the recipe context
// functions available as globals. Returns the script's return value.
func (c *RecipeContext) RunScript(script string) (goja.Value, error) {
	vm := goja.New()
	c.vm = vm

	// Expose context methods as global functions.
	vm.Set("FileExists", c.FileExists)
	vm.Set("ReadFile", c.ReadFile)
	vm.Set("ReadJSON", c.ReadJSON)
	vm.Set("ListFiles", c.ListFiles)
	vm.Set("SetPort", c.SetPort)
	vm.Set("SetEnv", c.SetEnv)
	vm.Set("SetImage", c.SetImage)
	vm.Set("SetBuildCommand", c.SetBuildCommand)
	vm.Set("SetStartCommand", c.SetStartCommand)

	val, err := vm.RunString(script)
	if err != nil {
		return nil, fmt.Errorf("run recipe script: %w", err)
	}
	return val, nil
}
