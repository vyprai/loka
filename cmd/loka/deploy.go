package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/vyprai/loka/internal/compose"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/recipe"
	"github.com/vyprai/loka/pkg/lokaapi"
	"github.com/vyprai/loka/internal/secret"
	"gopkg.in/yaml.v3"
)

// lokaYAML represents the loka.yaml project config file.
type lokaYAML struct {
	Name      string            `yaml:"name"`
	Recipe    string            `yaml:"recipe"`
	Domain    string            `yaml:"domain"`
	Port      int               `yaml:"port"`
	Image     string            `yaml:"image"`
	Build     []string          `yaml:"build"`
	Include   []string          `yaml:"include"`
	Start     string            `yaml:"start"`
	Env       map[string]string `yaml:"env"`

	// Routes and mounts.
	Routes []loka.ServiceRoute `yaml:"routes,omitempty"`
	Mounts []loka.Volume       `yaml:"mounts,omitempty"`

	// Health check config.
	HealthPath     string `yaml:"health_path,omitempty"`
	HealthInterval int    `yaml:"health_interval,omitempty"`
	HealthTimeout  int    `yaml:"health_timeout,omitempty"`
	HealthRetries  int    `yaml:"health_retries,omitempty"`

	// Autoscale config.
	Autoscale *loka.AutoscaleConfig `yaml:"autoscale,omitempty"`

	// Idle timeout in seconds (0 = never idle).
	IdleTimeout int `yaml:"idle_timeout,omitempty"`

	// Network ACL: which external services/databases this service can access.
	// Supports list form (["mydb"]) or map form ({"db": "mydb"}).
	Uses interface{} `yaml:"uses,omitempty"`

	// Multi-component service.
	Components map[string]lokaComponentYAML `yaml:"components,omitempty"`
}

// lokaComponentYAML represents one component in a multi-component loka.yaml.
type lokaComponentYAML struct {
	// Inline definition.
	Image     string            `yaml:"image,omitempty"`
	Port      int               `yaml:"port,omitempty"`
	Build     []string          `yaml:"build,omitempty"`
	Start     string            `yaml:"start,omitempty"`
	Domain    string            `yaml:"domain,omitempty"`
	Env       map[string]string `yaml:"env,omitempty"`
	Include   []string          `yaml:"include,omitempty"`
	Mounts    []loka.Volume     `yaml:"mounts,omitempty"`
	DependsOn []string          `yaml:"depends_on,omitempty"`
	Uses      interface{}       `yaml:"uses,omitempty"`
	Command   string            `yaml:"command,omitempty"`

	// Monorepo path reference.
	Path string `yaml:"path,omitempty"`
}

// waitForServiceReady polls a service until it reaches running/error state.
// Returns the final status or error.
func waitForServiceReady(ctx context.Context, client *lokaapi.Client, serviceID string, sp *spinner) (string, error) {
	lastMsg := ""
	for i := 0; i < 180; i++ {
		time.Sleep(1 * time.Second)
		var updated struct {
			ID            string `json:"ID"`
			Status        string `json:"Status"`
			Ready         bool   `json:"Ready"`
			StatusMessage string `json:"StatusMessage"`
		}
		if err := client.Raw(ctx, "GET", "/api/v1/services/"+serviceID, nil, &updated); err != nil {
			continue
		}
		if updated.StatusMessage != "" && updated.StatusMessage != lastMsg {
			sp.update(updated.StatusMessage)
			lastMsg = updated.StatusMessage
		}
		if updated.Ready || updated.Status == "running" {
			sp.stop("Running")
			return updated.Status, nil
		}
		if updated.Status == "error" {
			sp.fail(updated.StatusMessage)
			return updated.Status, fmt.Errorf("deployment failed: %s", updated.StatusMessage)
		}
	}
	sp.fail("Timed out")
	return "", fmt.Errorf("timed out waiting for service")
}

// isImageRef returns true if the argument looks like a Docker image reference
// (e.g., "nginx:latest", "ghcr.io/org/app:v2") rather than a local path.
func isImageRef(s string) bool {
	// Local paths start with . / ~ or are absolute
	if s == "." || s == ".." || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") ||
		strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~") {
		return false
	}
	// If it exists as a local directory, it's a path
	if info, err := os.Stat(s); err == nil && info.IsDir() {
		return false
	}
	// Image refs contain : (tag) or / (registry/org) or are well-known names
	return strings.Contains(s, ":") || strings.Contains(s, "/") || isWellKnownImage(s)
}

func isWellKnownImage(s string) bool {
	known := []string{"nginx", "postgres", "redis", "mysql", "mongo", "node", "python", "golang", "alpine", "ubuntu", "debian"}
	for _, k := range known {
		if s == k {
			return true
		}
	}
	return false
}

func newDeployCmd() *cobra.Command {
	var (
		recipeName string
		name       string
		domain     string
		port       int
		wait       bool
		envVars    []string
		command    string
	)

	cmd := &cobra.Command{
		Use:   "deploy [dir|image]",
		Short: "Deploy an application or Docker image",
		Long: `Build and deploy an application to LOKA.

Auto-detects the deployment mode:
  - Docker image:      loka deploy nginx:latest
  - Docker Compose:    loka deploy (with docker-compose.yml)
  - Multi-component:   loka deploy (with components: in loka.yaml)
  - Source project:     loka deploy (with recipe auto-detection)

Examples:
  loka deploy                                # Deploy current directory
  loka deploy nginx:latest --port 80         # Deploy Docker image
  loka deploy . --recipe nextjs              # Explicit recipe
  loka deploy . --name my-app --domain my-app`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) > 0 {
				dir = args[0]
			}

			// Docker image deploy: skip recipe/build/bundle
			if isImageRef(dir) {
				return deployImage(cmd, dir, name, domain, port, envVars, command, wait)
			}

			projectDir, err := filepath.Abs(dir)
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
				step("Loaded loka.yaml")
			}

			// 2. Check for multi-component deploy.
			if len(cfg.Components) > 0 {
				return deployMultiComponent(cmd, projectDir, cfg, name, wait)
			}

			// 3. Check for Docker Compose file.
			composeFile := compose.FindComposeFile(projectDir)
			if composeFile != "" {
				return deployCompose(cmd, composeFile, projectDir, name, wait)
			}

			// 4. Load .env.loka if present.
			envFile := filepath.Join(projectDir, ".env.loka")
			fileEnv := loadEnvFile(envFile)

			// 5. Detect recipe.
			var r *recipe.Recipe
			explicitRecipe := recipeName
			if explicitRecipe == "" {
				explicitRecipe = cfg.Recipe
			}

			if explicitRecipe != "" {
				r, err = recipe.LoadBuiltin(explicitRecipe)
				if err != nil {
					userDir := filepath.Join(recipe.UserRecipesDir(), explicitRecipe)
					r, err = recipe.LoadFromDir(userDir)
					if err != nil {
						return fmt.Errorf("recipe %q not found", explicitRecipe)
					}
				}
				step(fmt.Sprintf("Recipe: %s", r.Name))
			} else {
				sp := startSpinner("Detecting project type")
				r, err = recipe.Match(projectDir)
				if err != nil {
					sp.fail("Detection failed")
					return fmt.Errorf("recipe detection failed: %w", err)
				}
				if r == nil {
					sp.fail("Unknown project type")
					return fmt.Errorf("could not detect project type. Use --recipe to specify one.\nAvailable: loka recipe list")
				}
				sp.stop(fmt.Sprintf("Detected: %s%s%s", bold, r.Name, reset))
			}

			// 4. Merge config: CLI flags > loka.yaml > recipe defaults.
			finalName := coalesce(name, cfg.Name, filepath.Base(projectDir))
			finalDomain := coalesce(domain, cfg.Domain, finalName)
			// If domain is a bare name (no dot), append .loka TLD for local routing.
			if !strings.Contains(finalDomain, ".") {
				finalDomain = finalDomain + ".loka"
			}

			// For .loka domains on local spaces, check if DNS is enabled.
			if strings.HasSuffix(finalDomain, ".loka") && !isDNSEnabled() {
				fmt.Printf("  Deploy as %s%s%s? This requires DNS setup (sudo for /etc/resolver).\n", bold, finalDomain, reset)
				fmt.Printf("  [Y/n] ")
				reader := bufio.NewReader(os.Stdin)
				answer, _ := reader.ReadString('\n')
				answer = strings.TrimSpace(strings.ToLower(answer))
				if answer == "" || answer == "y" || answer == "yes" {
					fmt.Printf("  Setting up DNS for .loka...\n")
					if enableDNS() {
						step("DNS enabled")
					} else {
						fmt.Printf("  %s✗%s DNS setup failed. Falling back to port-based access.\n", red, reset)
						finalDomain = ""
					}
				} else {
					fmt.Printf("  Skipping DNS. Service will be available via port only.\n")
					finalDomain = ""
				}
			}
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

			// Validate all secret references exist before attempting deploy.
			secretStore := secret.NewStore()
			var missingSecrets []string
			for k, v := range finalEnv {
				if strings.Contains(v, "${secret.") {
					if _, err := secretStore.Resolve(v); err != nil {
						missingSecrets = append(missingSecrets, fmt.Sprintf("  %s: %v", k, err))
					}
				}
			}
			if len(missingSecrets) > 0 {
				return fmt.Errorf("missing secrets:\n%s\n\nAdd them with: loka secret set <name> <value>", strings.Join(missingSecrets, "\n"))
			}

			// Resolve ${secret.*} references in env values.
			for k, v := range finalEnv {
				resolved, err := secretStore.Resolve(v)
				if err != nil {
					return fmt.Errorf("resolve secret in env %s: %w", k, err)
				}
				finalEnv[k] = resolved
			}

			header("Configuration")
			kvPrint("Name", finalName)
			kvPrint("Image", finalImage)
			kvPrint("Port", fmt.Sprintf("%d", finalPort))
			kvPrint("Domain", finalDomain)
			fmt.Println()

			// 5. Run build commands locally.
			if len(buildCmds) > 0 {
				for _, buildCmd := range buildCmds {
					sp := startSpinner(fmt.Sprintf("Building: %s%s%s", dim, buildCmd, reset))
					if err := runBuildCommand(projectDir, buildCmd); err != nil {
						sp.fail(fmt.Sprintf("Build failed: %s", buildCmd))
						return fmt.Errorf("build failed: %w", err)
					}
					sp.stop(fmt.Sprintf("Built: %s", buildCmd))
				}
			}

			// 6. Bundle project into tar.gz.
			// Bundle: use recipe includes if specified, otherwise bundle entire project.
			// Fall back to bundling everything if recipe includes produce a near-empty bundle.
			includePaths := r.Include
			if len(includePaths) == 0 {
				includePaths = []string{"."}
			}
			sp := startSpinner("Bundling")
			bundlePath, err := createBundle(projectDir, includePaths)
			if err != nil {
				sp.fail("Bundle failed")
				return fmt.Errorf("bundle failed: %w", err)
			}
			defer os.Remove(bundlePath)

			bundleInfo, _ := os.Stat(bundlePath)
			// Fall back to bundling everything if recipe includes produced a tiny bundle.
			if bundleInfo.Size() < 1024 && len(r.Include) > 0 {
				os.Remove(bundlePath)
				bundlePath, err = createBundle(projectDir, []string{"."})
				if err != nil {
					sp.fail("Bundle failed")
					return fmt.Errorf("bundle failed: %w", err)
				}
				bundleInfo, _ = os.Stat(bundlePath)
			}
			sp.stop(fmt.Sprintf("Bundled (%.1f MB)", float64(bundleInfo.Size())/(1024*1024)))

			// 7. Upload bundle to objstore.
			serviceID := uuid.New().String()
			bundleKey := fmt.Sprintf("services/%s/bundle.tar.gz", serviceID)

			sp = startSpinner("Uploading bundle")
			client := newClient()
			bundleFile, err := os.Open(bundlePath)
			if err != nil {
				sp.fail("Upload failed")
				return fmt.Errorf("open bundle: %w", err)
			}
			defer bundleFile.Close()

			uploadPath := fmt.Sprintf("/api/v1/objstore/objects/%s", bundleKey)
			if err := client.UploadRaw(cmd.Context(), uploadPath, bundleFile, bundleInfo.Size()); err != nil {
				sp.fail("Upload failed")
				return fmt.Errorf("upload failed: %w", err)
			}
			sp.stop("Uploaded")

			// 8. Call POST /api/v1/services.

			// Send start command as-is — the supervisor runs it via sh -c
			// which handles quoting, pipes, and shell syntax correctly.
			startCmd := finalStart
			var startArgs []string

			var svc struct {
				ID            string `json:"ID"`
				Name          string `json:"Name"`
				Status        string `json:"Status"`
				Ready         bool   `json:"Ready"`
				StatusMessage string `json:"StatusMessage"`
			}

			// Build routes: prefer loka.yaml routes, fall back to domain default.
			var finalRoutes []map[string]any
			if len(cfg.Routes) > 0 {
				for _, rt := range cfg.Routes {
					route := map[string]any{"port": rt.Port}
					if rt.Domain != "" {
						route["domain"] = rt.Domain
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
					{"domain": finalDomain, "port": finalPort},
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

			// Health check: recipe defaults < loka.yaml overrides.
			if r.Health.Path != "" {
				deployReq["health_path"] = r.Health.Path
			}
			if r.Health.Interval > 0 {
				deployReq["health_interval"] = r.Health.Interval
			}
			if r.Health.Timeout > 0 {
				deployReq["health_timeout"] = r.Health.Timeout
			}
			if r.Health.Retries > 0 {
				deployReq["health_retries"] = r.Health.Retries
			}

			// Pass through optional fields from loka.yaml (overrides recipe).
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
			if cfg.Uses != nil {
				deployReq["uses"] = loka.NormalizeUses(cfg.Uses)
			}

			// Always use client-side polling (not ?wait=true) so we can show progress.
			sp = startSpinner("Creating service")
			if err := client.Raw(cmd.Context(), "POST", "/api/v1/services", deployReq, &svc); err != nil {
				sp.fail("Deploy failed")
				return fmt.Errorf("deploy failed: %w", err)
			}
			sp.stop("Service created")

			if wait && !svc.Ready {
				sp = startSpinner("Starting")
				lastMsg := ""
				for i := 0; i < 180; i++ {
					time.Sleep(1 * time.Second)
					var updated struct {
						ID            string `json:"ID"`
						Status        string `json:"Status"`
						Ready         bool   `json:"Ready"`
						StatusMessage string `json:"StatusMessage"`
					}
					if err := client.Raw(cmd.Context(), "GET", "/api/v1/services/"+svc.ID, nil, &updated); err != nil {
						continue
					}
					if updated.StatusMessage != "" && updated.StatusMessage != lastMsg {
						sp.update(updated.StatusMessage)
						lastMsg = updated.StatusMessage
					}
					if updated.Ready || updated.Status == "running" {
						svc.Status = updated.Status
						svc.Ready = updated.Ready
						sp.stop("Running")
						break
					}
					if updated.Status == "error" {
						sp.fail(updated.StatusMessage)
						return fmt.Errorf("deployment failed: %s", updated.StatusMessage)
					}
				}
				if !svc.Ready && svc.Status != "running" {
					sp.fail("Timed out waiting for service")
				}
			}

			// 9. Print result.
			displayName := svc.Name
			if displayName == "" {
				displayName = shortID(svc.ID)
			}
			header(fmt.Sprintf("Service: %s", displayName))
			kvPrint("Status", fmt.Sprintf("%s%s%s", green, svc.Status, reset))
			if finalDomain != "" {
				kvPrint("URL", fmt.Sprintf("https://%s", finalDomain))
			}
			fmt.Println()
			fmt.Printf("  %sloka service logs %s%s\n", dim, displayName, reset)
			fmt.Printf("  %sloka service stop %s%s\n", dim, displayName, reset)

			return nil
		},
	}

	cmd.Flags().StringVar(&recipeName, "recipe", "", "Recipe name (auto-detect if not specified)")
	cmd.Flags().StringVar(&name, "name", "", "Service name (default: directory or image name)")
	cmd.Flags().StringVar(&domain, "domain", "", "Domain (default: service name)")
	cmd.Flags().IntVar(&port, "port", 0, "Port override")
	cmd.Flags().BoolVar(&wait, "wait", true, "Wait for service to be ready")
	cmd.Flags().StringArrayVar(&envVars, "env", nil, "Environment variable (KEY=VALUE, repeatable)")
	cmd.Flags().StringVar(&command, "command", "", "Override start command")

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

// deployCompose deploys from a docker-compose.yml as a multi-component service.
func deployCompose(cmd *cobra.Command, composeFile, projectDir, name string, wait bool) error {
	step(fmt.Sprintf("Loaded %s", filepath.Base(composeFile)))

	cf, err := compose.Parse(composeFile)
	if err != nil {
		return fmt.Errorf("parse compose file: %w", err)
	}

	if name == "" {
		name = filepath.Base(projectDir)
	}

	header(fmt.Sprintf("Service: %s (%d components)", name, len(cf.Services)))

	components := cf.ToComponents()
	order := cf.DeployOrder()

	client := newClient()

	// Build component list for the service.
	var svcComponents []map[string]any
	for _, compName := range order {
		var comp *compose.Component
		for i := range components {
			if components[i].Name == compName {
				comp = &components[i]
				break
			}
		}
		if comp == nil {
			continue
		}

		c := map[string]any{
			"name":  comp.Name,
			"image": comp.Image,
			"port":  comp.Port,
			"env":   comp.Env,
		}
		if comp.Command != "" {
			c["command"] = comp.Command
		}
		if comp.HostPort > 0 {
			c["domain"] = fmt.Sprintf("%s.%s.loka", comp.Name, name)
		}
		if len(comp.DependsOn) > 0 {
			c["depends_on"] = comp.DependsOn
		}
		svcComponents = append(svcComponents, c)

		kvPrint(comp.Name, fmt.Sprintf("%s :%d", comp.Image, comp.Port))
	}
	fmt.Println()

	// Create service with components.
	sp := startSpinner("Deploying")
	deployReq := map[string]any{
		"name":       name,
		"image":      components[0].Image, // Primary image.
		"port":       components[0].Port,
		"components": svcComponents,
	}

	var svc struct {
		ID     string `json:"ID"`
		Name   string `json:"Name"`
		Status string `json:"Status"`
	}
	if err := client.Raw(cmd.Context(), "POST", "/api/v1/services", deployReq, &svc); err != nil {
		sp.fail("Deploy failed")
		return fmt.Errorf("deploy failed: %w", err)
	}
	sp.stop("Service created")

	if wait {
		sp = startSpinner("Starting components")
		for i := 0; i < 180; i++ {
			time.Sleep(1 * time.Second)
			var updated struct {
				Status        string `json:"Status"`
				Ready         bool   `json:"Ready"`
				StatusMessage string `json:"StatusMessage"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/services/"+svc.ID, nil, &updated); err != nil {
				continue
			}
			if updated.StatusMessage != "" {
				sp.update(updated.StatusMessage)
			}
			if updated.Ready || updated.Status == "running" {
				sp.stop("All components running")
				break
			}
			if updated.Status == "error" {
				sp.fail(updated.StatusMessage)
				return fmt.Errorf("deployment failed: %s", updated.StatusMessage)
			}
		}
	}

	header(fmt.Sprintf("Service: %s", name))
	for _, c := range svcComponents {
		domain := c["domain"]
		if domain == nil || domain == "" {
			domain = "(internal)"
		}
		fmt.Printf("  %s%-10s%s %s → %v\n", dim, c["name"], reset, c["image"], domain)
	}
	fmt.Println()

	return nil
}

// deployMultiComponent deploys a multi-component service from loka.yaml.
func deployMultiComponent(cmd *cobra.Command, projectDir string, cfg lokaYAML, name string, wait bool) error {
	if name == "" {
		name = cfg.Name
	}
	if name == "" {
		name = filepath.Base(projectDir)
	}

	header(fmt.Sprintf("Service: %s (%d components)", name, len(cfg.Components)))

	client := newClient()
	var svcComponents []map[string]any

	for compName, comp := range cfg.Components {
		c := map[string]any{
			"name": compName,
			"port": comp.Port,
		}

		if comp.Path != "" {
			// Monorepo: resolve from subdirectory.
			kvPrint(compName, fmt.Sprintf("path: %s", comp.Path))
			c["path"] = comp.Path
		} else {
			// Inline component.
			c["image"] = comp.Image
			if comp.Command != "" {
				c["command"] = comp.Command
			}
			if comp.Start != "" {
				c["command"] = comp.Start
			}
			if comp.Domain != "" {
				c["domain"] = comp.Domain
			}
			if len(comp.Env) > 0 {
				c["env"] = comp.Env
			}
			if len(comp.DependsOn) > 0 {
				c["depends_on"] = comp.DependsOn
			}
			kvPrint(compName, fmt.Sprintf("%s :%d", comp.Image, comp.Port))
		}

		svcComponents = append(svcComponents, c)
	}
	fmt.Println()

	// For components with build steps, build and bundle each one.
	for compName, comp := range cfg.Components {
		if len(comp.Build) > 0 {
			buildDir := projectDir
			if comp.Path != "" {
				buildDir = filepath.Join(projectDir, comp.Path)
			}
			for _, buildCmd := range comp.Build {
				sp := startSpinner(fmt.Sprintf("[%s] %s", compName, buildCmd))
				if err := runBuildCommand(buildDir, buildCmd); err != nil {
					sp.fail(fmt.Sprintf("[%s] Build failed", compName))
					return fmt.Errorf("build %s failed: %w", compName, err)
				}
				sp.stop(fmt.Sprintf("[%s] Built", compName))
			}
		}
	}

	// Create the service.
	sp := startSpinner("Deploying")
	firstComp := svcComponents[0]
	deployReq := map[string]any{
		"name":       name,
		"image":      firstComp["image"],
		"port":       firstComp["port"],
		"components": svcComponents,
	}
	if cfg.Domain != "" {
		deployReq["routes"] = []map[string]any{{"domain": cfg.Domain, "port": firstComp["port"]}}
	}

	var svc struct {
		ID     string `json:"ID"`
		Status string `json:"Status"`
	}
	if err := client.Raw(cmd.Context(), "POST", "/api/v1/services", deployReq, &svc); err != nil {
		sp.fail("Deploy failed")
		return fmt.Errorf("deploy failed: %w", err)
	}
	sp.stop("Service created")

	if wait {
		sp = startSpinner("Starting components")
		for i := 0; i < 180; i++ {
			time.Sleep(1 * time.Second)
			var updated struct {
				Status        string `json:"Status"`
				Ready         bool   `json:"Ready"`
				StatusMessage string `json:"StatusMessage"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/services/"+svc.ID, nil, &updated); err != nil {
				continue
			}
			if updated.StatusMessage != "" {
				sp.update(updated.StatusMessage)
			}
			if updated.Ready || updated.Status == "running" {
				sp.stop("All components running")
				break
			}
			if updated.Status == "error" {
				sp.fail(updated.StatusMessage)
				return fmt.Errorf("deployment failed: %s", updated.StatusMessage)
			}
		}
	}

	header(fmt.Sprintf("Service: %s", name))
	kvPrint("Status", fmt.Sprintf("%srunning%s", green, reset))
	if cfg.Domain != "" {
		kvPrint("URL", fmt.Sprintf("https://%s", cfg.Domain))
	}
	fmt.Println()

	return nil
}

// deployImage deploys a Docker image directly without recipe detection or bundling.
func deployImage(cmd *cobra.Command, imageRef, name, domain string, port int, envVars []string, command string, wait bool) error {
	// Derive name from image ref if not specified.
	if name == "" {
		name = imageRef
		// Strip registry prefix and tag.
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		if i := strings.Index(name, ":"); i >= 0 {
			name = name[:i]
		}
	}

	if port == 0 {
		port = 80
	}

	// Build domain.
	finalDomain := domain
	if finalDomain == "" {
		finalDomain = name
	}
	if !strings.Contains(finalDomain, ".") {
		finalDomain = finalDomain + ".loka"
	}

	// DNS check.
	if strings.HasSuffix(finalDomain, ".loka") && !isDNSEnabled() {
		fmt.Printf("  Deploy as %s%s%s? This requires DNS setup (sudo for /etc/resolver).\n", bold, finalDomain, reset)
		fmt.Printf("  [Y/n] ")
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer == "" || answer == "y" || answer == "yes" {
			fmt.Printf("  Setting up DNS for .loka...\n")
			if enableDNS() {
				step("DNS enabled")
			} else {
				fmt.Printf("  %s✗%s DNS setup failed. Falling back to port-based access.\n", red, reset)
				finalDomain = ""
			}
		} else {
			finalDomain = ""
		}
	}

	// Parse env vars.
	env := make(map[string]string)
	for _, e := range envVars {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			env[parts[0]] = parts[1]
		}
	}

	header("Configuration")
	kvPrint("Name", name)
	kvPrint("Image", imageRef)
	kvPrint("Port", fmt.Sprintf("%d", port))
	if finalDomain != "" {
		kvPrint("Domain", finalDomain)
	}
	if command != "" {
		kvPrint("Command", command)
	}
	fmt.Println()

	// Deploy — no bundle, just image.
	sp := startSpinner("Creating service")
	client := newClient()

	var routes []map[string]any
	if finalDomain != "" {
		routes = []map[string]any{{"domain": finalDomain, "port": port}}
	}

	deployReq := map[string]any{
		"name":  name,
		"image": imageRef,
		"port":  port,
		"env":   env,
	}
	if command != "" {
		deployReq["command"] = command
	}
	if len(routes) > 0 {
		deployReq["routes"] = routes
	}

	var svc struct {
		ID            string `json:"ID"`
		Name          string `json:"Name"`
		Status        string `json:"Status"`
		Ready         bool   `json:"Ready"`
		StatusMessage string `json:"StatusMessage"`
	}
	if err := client.Raw(cmd.Context(), "POST", "/api/v1/services", deployReq, &svc); err != nil {
		sp.fail("Deploy failed")
		return fmt.Errorf("deploy failed: %w", err)
	}
	sp.stop("Service created")

	if wait && !svc.Ready {
		sp = startSpinner("Starting")
		for i := 0; i < 180; i++ {
			time.Sleep(1 * time.Second)
			var updated struct {
				ID            string `json:"ID"`
				Status        string `json:"Status"`
				Ready         bool   `json:"Ready"`
				StatusMessage string `json:"StatusMessage"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/services/"+svc.ID, nil, &updated); err != nil {
				continue
			}
			if updated.StatusMessage != "" {
				sp.update(updated.StatusMessage)
			}
			if updated.Ready || updated.Status == "running" {
				svc.Status = updated.Status
				sp.stop("Running")
				break
			}
			if updated.Status == "error" {
				sp.fail(updated.StatusMessage)
				return fmt.Errorf("deployment failed: %s", updated.StatusMessage)
			}
		}
	}

	displayName := svc.Name
	if displayName == "" {
		displayName = shortID(svc.ID)
	}
	header(fmt.Sprintf("Service: %s", displayName))
	kvPrint("Status", fmt.Sprintf("%s%s%s", green, svc.Status, reset))
	kvPrint("Image", imageRef)
	if finalDomain != "" {
		kvPrint("URL", fmt.Sprintf("https://%s", finalDomain))
	}
	fmt.Println()
	fmt.Printf("  %sloka service logs %s%s\n", dim, displayName, reset)
	fmt.Printf("  %sloka service stop %s%s\n", dim, displayName, reset)

	return nil
}



