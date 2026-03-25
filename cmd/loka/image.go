package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/vyprai/loka/internal/registry"
	"github.com/vyprai/loka/internal/secret"
)

func newImageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Manage container images",
		Long: `Pull, list, inspect, and manage container images in the local registry.

Examples:
  loka image pull node:20-slim
  loka image list
  loka image inspect node:20-slim
  loka image delete node:20-slim
  loka image layers`,
	}

	cmd.AddCommand(
		newImagePullCmd(),
		newImageListCmd(),
		newImageInspectCmd(),
		newImageDeleteCmd(),
		newImageLayersCmd(),
		newImageRegistryCmd(),
	)
	return cmd
}

func newImagePullCmd() *cobra.Command {
	var registryName string

	cmd := &cobra.Command{
		Use:   "pull <image>",
		Short: "Pull a container image from a remote registry",
		Long: `Pull a container image from a remote registry into the local store.

Examples:
  loka image pull node:20-slim
  loka image pull ubuntu:22.04
  loka image pull ghcr.io/org/app:latest --registry ghcr`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]

			// Resolve auth from registry config.
			auth, err := resolveImageAuth(registryName, ref)
			if err != nil {
				return fmt.Errorf("resolve auth: %w", err)
			}

			// Use the control plane API to pull.
			client := newClient()
			var resp struct {
				Message  string `json:"message"`
				Manifest any    `json:"manifest"`
			}

			body := map[string]any{
				"image": ref,
			}
			if auth != nil && auth.Token != "" {
				body["token"] = auth.Token
			}
			if auth != nil && auth.Username != "" {
				body["username"] = auth.Username
				body["password"] = auth.Password
			}

			fmt.Printf("Pulling %s...\n", ref)
			if err := client.Raw(cmd.Context(), "POST", "/api/v1/images/registry/pull", body, &resp); err != nil {
				return fmt.Errorf("pull failed: %w", err)
			}

			reg, repo, tag := registry.ParseReference(ref)
			_ = reg
			fmt.Printf("Successfully pulled %s:%s\n", repo, tag)
			return nil
		},
	}

	cmd.Flags().StringVar(&registryName, "registry", "", "Registry to use (default: auto-detect from image reference)")
	return cmd
}

func newImageListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List images in the local registry",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()

			var resp struct {
				Repositories []string `json:"repositories"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/images/registry/catalog", nil, &resp); err != nil {
				return fmt.Errorf("list images: %w", err)
			}

			if len(resp.Repositories) == 0 {
				fmt.Println("No images found. Pull an image with: loka image pull <image>")
				return nil
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "REPOSITORY\tTAGS")
			for _, repo := range resp.Repositories {
				tags := listImageTags(cmd.Context(), client, repo)
				fmt.Fprintf(tw, "%s\t%s\n", repo, tags)
			}
			tw.Flush()
			return nil
		},
	}
}

func newImageInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <image>",
		Short: "Show details of an image",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			_, repo, tag := registry.ParseReference(ref)

			client := newClient()
			var manifest registry.OCIManifest
			path := fmt.Sprintf("/api/v1/images/registry/manifests/%s/%s", repo, tag)
			if err := client.Raw(cmd.Context(), "GET", path, nil, &manifest); err != nil {
				return fmt.Errorf("inspect image: %w", err)
			}

			// Pretty print.
			data, _ := json.MarshalIndent(manifest, "", "  ")
			fmt.Println(string(data))
			return nil
		},
	}
}

func newImageDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <image>",
		Short: "Delete an image from the local registry",
		Aliases: []string{"rm"},
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			_, repo, tag := registry.ParseReference(ref)

			client := newClient()
			path := fmt.Sprintf("/api/v1/images/registry/manifests/%s/%s", repo, tag)
			if err := client.Raw(cmd.Context(), "DELETE", path, nil, nil); err != nil {
				return fmt.Errorf("delete image: %w", err)
			}

			fmt.Printf("Deleted %s:%s\n", repo, tag)
			return nil
		},
	}
}

func newImageLayersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "layers",
		Short: "List all stored layers (blobs)",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()

			var resp struct {
				Blobs []string `json:"blobs"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/images/registry/blobs", nil, &resp); err != nil {
				return fmt.Errorf("list layers: %w", err)
			}

			if len(resp.Blobs) == 0 {
				fmt.Println("No layers stored.")
				return nil
			}

			fmt.Printf("DIGEST (%d layers)\n", len(resp.Blobs))
			for _, digest := range resp.Blobs {
				fmt.Println(digest)
			}
			return nil
		},
	}
}

func newImageRegistryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Manage container registries",
	}

	cmd.AddCommand(
		newImageRegistryListCmd(),
		newImageRegistryAddCmd(),
		newImageRegistryRemoveCmd(),
		newImageRegistryDefaultCmd(),
	)
	return cmd
}

func newImageRegistryListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured registries",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			regs, err := registry.LoadRegistries()
			if err != nil {
				return fmt.Errorf("load registries: %w", err)
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tURL\tDEFAULT\tBUILTIN")
			for _, r := range regs {
				def := ""
				if r.Default {
					def = "*"
				}
				builtin := ""
				if r.Builtin {
					builtin = "yes"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, r.URL, def, builtin)
			}
			tw.Flush()
			return nil
		},
	}
}

func newImageRegistryAddCmd() *cobra.Command {
	var (
		url   string
		token string
	)

	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add a container registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			regs, err := registry.LoadRegistries()
			if err != nil {
				return fmt.Errorf("load registries: %w", err)
			}

			// Check for duplicate.
			for _, r := range regs {
				if r.Name == name {
					return fmt.Errorf("registry %q already exists", name)
				}
			}

			regs = append(regs, registry.RegistryConfig{
				Name:  name,
				URL:   url,
				Token: token,
			})

			if err := registry.SaveRegistries(regs); err != nil {
				return fmt.Errorf("save registries: %w", err)
			}

			fmt.Printf("Added registry %q (%s)\n", name, url)
			return nil
		},
	}

	cmd.Flags().StringVar(&url, "url", "", "Registry URL (required)")
	cmd.Flags().StringVar(&token, "token", "", "Authentication token")
	cmd.MarkFlagRequired("url")
	return cmd
}

func newImageRegistryRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a container registry",
		Aliases: []string{"rm"},
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			regs, err := registry.LoadRegistries()
			if err != nil {
				return fmt.Errorf("load registries: %w", err)
			}

			found := false
			filtered := make([]registry.RegistryConfig, 0, len(regs))
			for _, r := range regs {
				if r.Name == name {
					if r.Builtin {
						return fmt.Errorf("cannot remove built-in registry %q", name)
					}
					found = true
					continue
				}
				filtered = append(filtered, r)
			}
			if !found {
				return fmt.Errorf("registry %q not found", name)
			}

			if err := registry.SaveRegistries(filtered); err != nil {
				return fmt.Errorf("save registries: %w", err)
			}

			fmt.Printf("Removed registry %q\n", name)
			return nil
		},
	}
}

func newImageRegistryDefaultCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "default <name>",
		Short: "Set the default registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			regs, err := registry.LoadRegistries()
			if err != nil {
				return fmt.Errorf("load registries: %w", err)
			}

			found := false
			for i := range regs {
				if regs[i].Name == name {
					found = true
					regs[i].Default = true
				} else {
					regs[i].Default = false
				}
			}
			if !found {
				return fmt.Errorf("registry %q not found", name)
			}

			if err := registry.SaveRegistries(regs); err != nil {
				return fmt.Errorf("save registries: %w", err)
			}

			fmt.Printf("Default registry set to %q\n", name)
			return nil
		},
	}
}

// resolveImageAuth resolves authentication credentials for pulling an image.
func resolveImageAuth(registryName, ref string) (*registry.AuthConfig, error) {
	regs, err := registry.LoadRegistries()
	if err != nil {
		return &registry.AuthConfig{}, nil
	}

	secretStore := secret.NewStore()

	// If registry name given, find it.
	if registryName != "" {
		for _, r := range regs {
			if r.Name == registryName {
				return registry.ResolveAuth(r, secretStore)
			}
		}
		return nil, fmt.Errorf("registry %q not found", registryName)
	}

	// Auto-detect from image reference.
	reg, _, _ := registry.ParseReference(ref)
	for _, r := range regs {
		// Match by URL hostname.
		if matchesRegistry(r.URL, reg) {
			return registry.ResolveAuth(r, secretStore)
		}
	}

	// No match — anonymous auth.
	return &registry.AuthConfig{}, nil
}

// matchesRegistry checks if a registry config URL matches a registry hostname.
func matchesRegistry(configURL, registryHost string) bool {
	// Strip scheme.
	url := configURL
	for _, prefix := range []string{"https://", "http://"} {
		if len(url) > len(prefix) && url[:len(prefix)] == prefix {
			url = url[len(prefix):]
			break
		}
	}
	// Strip trailing slash.
	if len(url) > 0 && url[len(url)-1] == '/' {
		url = url[:len(url)-1]
	}
	return url == registryHost
}

// listImageTags fetches tags for a repository and returns them as a comma-separated string.
func listImageTags(ctx context.Context, client interface{ Raw(context.Context, string, string, any, any) error }, repo string) string {
	var resp struct {
		Tags []string `json:"tags"`
	}
	path := fmt.Sprintf("/api/v1/images/registry/tags/%s", repo)
	if err := client.Raw(ctx, "GET", path, nil, &resp); err != nil {
		return "?"
	}
	if len(resp.Tags) == 0 {
		return "-"
	}
	result := ""
	for i, tag := range resp.Tags {
		if i > 0 {
			result += ", "
		}
		result += tag
		if i >= 4 {
			result += fmt.Sprintf(" (+%d more)", len(resp.Tags)-5)
			break
		}
	}
	return result
}
