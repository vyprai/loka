package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/recipe"
	"github.com/vyprai/loka/internal/secret"
	"gopkg.in/yaml.v3"
)

// lokaYAML represents the loka.yaml project config file.
type lokaYAML struct {
	Name      string            `yaml:"name"`
	Recipe    string            `yaml:"recipe"`
	Subdomain string            `yaml:"subdomain"`
	Port      int               `yaml:"port"`
	Image     string            `yaml:"image"`
	Build     []string          `yaml:"build"`
	Include   []string          `yaml:"include"`
	Start     string            `yaml:"start"`
	Env       map[string]string `yaml:"env"`

	// Routes and mounts.
	Routes []loka.ServiceRoute `yaml:"routes,omitempty"`
	Mounts []loka.VolumeMount  `yaml:"mounts,omitempty"`

	// Health check config.
	HealthPath     string `yaml:"health_path,omitempty"`
	HealthInterval int    `yaml:"health_interval,omitempty"`
	HealthTimeout  int    `yaml:"health_timeout,omitempty"`
	HealthRetries  int    `yaml:"health_retries,omitempty"`

	// Autoscale config.
	Autoscale *loka.AutoscaleConfig `yaml:"autoscale,omitempty"`

	// Idle timeout in seconds (0 = never idle).
	IdleTimeout int `yaml:"idle_timeout,omitempty"`
}

func newDeployCmd() *cobra.Command {
	var (
		recipeName string
		name       string
		subdomain  string
		port       int
		wait       bool
	)

	cmd := &cobra.Command{
		Use:   "deploy <dir>",
		Short: "Deploy an application",
		Long: `Build and deploy an application to LOKA.

Auto-detects the project type (Next.js, Vite, Go, Python, etc.) and builds
locally, then uploads the bundle and starts the service.

Examples:
  loka deploy .                              # Auto-detect recipe
  loka deploy . --recipe nextjs              # Explicit recipe
  loka deploy . --name my-app --subdomain my-app`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectDir, err := filepath.Abs(args[0])
			if err != nil {
				return fmt.Errorf("resolve path: %w", err)
			}

			// 1. Load loka.yaml if present.
			var cfg lokaYAML
			lokaYAMLPath := filepath.Join(projectDir, "loka.yaml")
			if data, err := os.ReadFile(lokaYAMLPath); err == nil {
				if err := yaml.Unmarshal(data, &cfg); err != nil {
					return fmt.Errorf("parse loka.yaml: %w", err)
				}
				fmt.Printf("Loaded loka.yaml\n")
			}

			// 2. Load .env.loka if present.
			envFile := filepath.Join(projectDir, ".env.loka")
			fileEnv := loadEnvFile(envFile)

			// 3. Detect recipe.
			var r *recipe.Recipe
			explicitRecipe := recipeName
			if explicitRecipe == "" {
				explicitRecipe = cfg.Recipe
			}

			if explicitRecipe != "" {
				// Try built-in first, then user-installed.
				r, err = recipe.LoadBuiltin(explicitRecipe)
				if err != nil {
					// Try from user recipes dir.
					userDir := filepath.Join(recipe.UserRecipesDir(), explicitRecipe)
					r, err = recipe.LoadFromDir(userDir)
					if err != nil {
						return fmt.Errorf("recipe %q not found", explicitRecipe)
					}
				}
				fmt.Printf("Using recipe: %s\n", r.Name)
			} else {
				fmt.Print("Detecting project type...")
				r, err = recipe.Match(projectDir)
				if err != nil {
					return fmt.Errorf("recipe detection failed: %w", err)
				}
				if r == nil {
					return fmt.Errorf("could not detect project type. Use --recipe to specify one.\nAvailable: loka recipe list")
				}
				fmt.Printf(" %s\n", r.Name)
			}

			// 4. Merge config: CLI flags > loka.yaml > recipe defaults.
			finalName := coalesce(name, cfg.Name, filepath.Base(projectDir))
			finalSubdomain := coalesce(subdomain, cfg.Subdomain, finalName)
			finalPort := r.Port
			if cfg.Port != 0 {
				finalPort = cfg.Port
			}
			if port != 0 {
				finalPort = port
			}
			if finalPort == 0 {
				finalPort = 8080
			}

			finalImage := coalesce(cfg.Image, r.Image)
			if finalImage == "" {
				finalImage = "node:20-slim" // sensible default
			}

			finalStart := r.Start
			if cfg.Start != "" {
				finalStart = cfg.Start
			}

			buildCmds := r.Build
			if len(cfg.Build) > 0 {
				buildCmds = cfg.Build
			}

			includePaths := r.Include
			if len(cfg.Include) > 0 {
				includePaths = cfg.Include
			}

			// Merge env: recipe defaults < .env.loka < loka.yaml < recipe env.
			finalEnv := make(map[string]string)
			for k, v := range r.Env {
				finalEnv[k] = v
			}
			for k, v := range fileEnv {
				finalEnv[k] = v
			}
			for k, v := range cfg.Env {
				finalEnv[k] = v
			}

			// Resolve ${secret.*} references in env values.
			secretStore := secret.NewStore()
			for k, v := range finalEnv {
				resolved, err := secretStore.Resolve(v)
				if err != nil {
					return fmt.Errorf("resolve secret in env %s: %w", k, err)
				}
				finalEnv[k] = resolved
			}

			fmt.Printf("\n")
			fmt.Printf("  Name:      %s\n", finalName)
			fmt.Printf("  Recipe:    %s\n", r.Name)
			fmt.Printf("  Image:     %s\n", finalImage)
			fmt.Printf("  Port:      %d\n", finalPort)
			fmt.Printf("  Subdomain: %s\n", finalSubdomain)
			fmt.Printf("\n")

			// 5. Run build commands locally.
			if len(buildCmds) > 0 {
				fmt.Println("Building...")
				for _, buildCmd := range buildCmds {
					fmt.Printf("  $ %s\n", buildCmd)
					if err := runBuildCommand(projectDir, buildCmd); err != nil {
						return fmt.Errorf("build failed: %w", err)
					}
				}
				fmt.Println("  Build complete.")
			}

			// 6. Bundle include paths into tar.gz.
			fmt.Print("Bundling...")
			bundlePath, err := createBundle(projectDir, includePaths)
			if err != nil {
				return fmt.Errorf("bundle failed: %w", err)
			}
			defer os.Remove(bundlePath)

			bundleInfo, _ := os.Stat(bundlePath)
			fmt.Printf(" %.1f MB\n", float64(bundleInfo.Size())/(1024*1024))

			// 7. Upload bundle to objstore.
			serviceID := uuid.New().String()
			bundleKey := fmt.Sprintf("services/%s/bundle.tar.gz", serviceID)

			fmt.Print("Uploading bundle...")
			client := newClient()
			bundleFile, err := os.Open(bundlePath)
			if err != nil {
				return fmt.Errorf("open bundle: %w", err)
			}
			defer bundleFile.Close()

			uploadPath := fmt.Sprintf("/api/v1/objstore/objects/%s", bundleKey)
			if err := client.UploadRaw(cmd.Context(), uploadPath, bundleFile, bundleInfo.Size()); err != nil {
				return fmt.Errorf("upload failed: %w", err)
			}
			fmt.Println(" done")

			// 8. Call POST /api/v1/services.
			fmt.Print("Deploying service...")

			// Parse start command into command + args.
			startParts := strings.Fields(finalStart)
			var startCmd string
			var startArgs []string
			if len(startParts) > 0 {
				startCmd = startParts[0]
				startArgs = startParts[1:]
			}

			var svc struct {
				ID            string `json:"ID"`
				Name          string `json:"Name"`
				Status        string `json:"Status"`
				Ready         bool   `json:"Ready"`
				StatusMessage string `json:"StatusMessage"`
			}

			// Build routes: prefer loka.yaml routes, fall back to subdomain default.
			var finalRoutes []map[string]any
			if len(cfg.Routes) > 0 {
				for _, rt := range cfg.Routes {
					route := map[string]any{"port": rt.Port}
					if rt.Subdomain != "" {
						route["subdomain"] = rt.Subdomain
					}
					if rt.CustomDomain != "" {
						route["custom_domain"] = rt.CustomDomain
					}
					if rt.Protocol != "" {
						route["protocol"] = rt.Protocol
					}
					finalRoutes = append(finalRoutes, route)
				}
			} else {
				finalRoutes = []map[string]any{
					{"subdomain": finalSubdomain, "port": finalPort},
				}
			}

			deployReq := map[string]any{
				"name":        finalName,
				"image":       finalImage,
				"recipe_name": r.Name,
				"command":     startCmd,
				"args":        startArgs,
				"env":         finalEnv,
				"port":        finalPort,
				"bundle_key":  bundleKey,
				"routes":      finalRoutes,
			}

			// Pass through optional fields from loka.yaml.
			if len(cfg.Mounts) > 0 {
				deployReq["mounts"] = cfg.Mounts
			}
			if cfg.HealthPath != "" {
				deployReq["health_path"] = cfg.HealthPath
			}
			if cfg.HealthInterval > 0 {
				deployReq["health_interval"] = cfg.HealthInterval
			}
			if cfg.HealthTimeout > 0 {
				deployReq["health_timeout"] = cfg.HealthTimeout
			}
			if cfg.HealthRetries > 0 {
				deployReq["health_retries"] = cfg.HealthRetries
			}
			if cfg.Autoscale != nil {
				deployReq["autoscale"] = cfg.Autoscale
			}
			if cfg.IdleTimeout > 0 {
				deployReq["idle_timeout"] = cfg.IdleTimeout
			}

			waitQuery := ""
			if wait {
				waitQuery = "?wait=true"
			}

			if err := client.Raw(cmd.Context(), "POST", "/api/v1/services"+waitQuery, deployReq, &svc); err != nil {
				return fmt.Errorf("deploy failed: %w", err)
			}

			if !wait && !svc.Ready {
				// Poll until running.
				fmt.Print(".")
				for i := 0; i < 120; i++ {
					time.Sleep(1 * time.Second)
					var updated struct {
						ID            string `json:"ID"`
						Status        string `json:"Status"`
						Ready         bool   `json:"Ready"`
						StatusMessage string `json:"StatusMessage"`
					}
					if err := client.Raw(cmd.Context(), "GET", "/api/v1/services/"+svc.ID, nil, &updated); err != nil {
						break
					}
					if updated.Ready || updated.Status == "running" {
						svc.Status = updated.Status
						svc.Ready = updated.Ready
						break
					}
					if updated.Status == "error" {
						fmt.Println(" FAILED")
						return fmt.Errorf("deployment failed: %s", updated.StatusMessage)
					}
					fmt.Print(".")
				}
			}
			fmt.Println(" ready!")

			// 9. Print URL.
			fmt.Printf("\n")
			fmt.Printf("Service deployed: %s\n", svc.ID)
			fmt.Printf("  Status:    %s\n", svc.Status)

			// Determine base domain from server endpoint.
			endpoint, _, _, _ := resolveServer()
			baseDomain := extractBaseDomain(endpoint)
			if baseDomain != "" {
				fmt.Printf("  URL:       https://%s.%s\n", finalSubdomain, baseDomain)
			}
			fmt.Printf("\n")
			fmt.Printf("Manage:\n")
			fmt.Printf("  loka service get %s\n", shortID(svc.ID))
			fmt.Printf("  loka service logs %s\n", shortID(svc.ID))
			fmt.Printf("  loka service stop %s\n", shortID(svc.ID))

			return nil
		},
	}

	cmd.Flags().StringVar(&recipeName, "recipe", "", "Recipe name (auto-detect if not specified)")
	cmd.Flags().StringVar(&name, "name", "", "Service name (default: directory name)")
	cmd.Flags().StringVar(&subdomain, "subdomain", "", "Subdomain (default: service name)")
	cmd.Flags().IntVar(&port, "port", 0, "Port override")
	cmd.Flags().BoolVar(&wait, "wait", true, "Wait for service to be ready")

	return cmd
}

// coalesce returns the first non-empty string.
func coalesce(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// loadEnvFile reads a .env file and returns key=value pairs.
func loadEnvFile(path string) map[string]string {
	env := make(map[string]string)
	data, err := os.ReadFile(path)
	if err != nil {
		return env
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		// Strip surrounding quotes.
		v = strings.Trim(v, `"'`)
		env[k] = v
	}
	return env
}

// runBuildCommand executes a shell command in the project directory.
func runBuildCommand(dir, command string) error {
	cmd := newShellExecCommand(command)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// newShellExecCommand creates an exec.Cmd that runs a command via the shell.
func newShellExecCommand(command string) *exec.Cmd {
	return exec.Command("sh", "-c", command)
}

// createBundle creates a tar.gz archive of the specified paths relative to projectDir.
func createBundle(projectDir string, includes []string) (string, error) {
	if len(includes) == 0 {
		// Default: bundle everything except common excludes.
		includes = []string{"."}
	}

	tmpFile, err := os.CreateTemp("", "loka-bundle-*.tar.gz")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	defer tmpFile.Close()

	gzw := gzip.NewWriter(tmpFile)
	defer gzw.Close()
	tw := tar.NewWriter(gzw)
	defer tw.Close()

	// Common excludes.
	excludes := map[string]bool{
		".git":         true,
		"node_modules": true,
		".next":        false, // .next is often needed for Next.js
		"__pycache__":  true,
		".venv":        true,
		"venv":         true,
	}

	for _, include := range includes {
		absInclude := filepath.Join(projectDir, include)

		err := filepath.Walk(absInclude, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // skip errors
			}

			// Check excludes.
			base := filepath.Base(path)
			if info.IsDir() && excludes[base] {
				return filepath.SkipDir
			}

			// Get path relative to project dir.
			relPath, err := filepath.Rel(projectDir, path)
			if err != nil {
				return nil
			}

			// Skip the root directory entry.
			if relPath == "." {
				return nil
			}

			header, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			header.Name = relPath

			if err := tw.WriteHeader(header); err != nil {
				return err
			}

			if !info.IsDir() {
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				defer f.Close()
				if _, err := io.Copy(tw, f); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("walk %s: %w", include, err)
		}
	}

	return tmpFile.Name(), nil
}

// extractBaseDomain tries to derive the base domain from the server endpoint.
func extractBaseDomain(endpoint string) string {
	// For local development, there is no base domain.
	if strings.Contains(endpoint, "localhost") || strings.Contains(endpoint, "127.0.0.1") {
		return "localhost"
	}
	// Strip protocol.
	host := endpoint
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	// Strip port.
	if idx := strings.Index(host, ":"); idx != -1 {
		host = host[:idx]
	}
	return host
}
