package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newVolumeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "volume",
		Short: "Manage named volumes",
		Long: `Create, list, inspect, and delete named volumes.

Named volumes persist data in the object store and can be mounted by
multiple sessions or services simultaneously.

Examples:
  loka volume list
  loka volume create my-data
  loka volume inspect my-data
  loka volume delete my-data`,
	}

	cmd.AddCommand(
		newVolumeListCmd(),
		newVolumeCreateCmd(),
		newVolumeInspectCmd(),
		newVolumeDeleteCmd(),
	)
	return cmd
}

func newVolumeListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List all named volumes",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()

			var resp struct {
				Volumes []struct {
					Name       string `json:"name"`
					Provider   string `json:"provider"`
					MountCount int    `json:"mount_count"`
					CreatedAt  string `json:"created_at"`
				} `json:"volumes"`
				Total int `json:"total"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/volumes", nil, &resp); err != nil {
				return fmt.Errorf("list volumes: %w", err)
			}

			if len(resp.Volumes) == 0 {
				fmt.Println("No volumes found. Create one with: loka volume create <name>")
				return nil
			}

			if outputFmt == "json" {
				data, _ := json.MarshalIndent(resp, "", "  ")
				fmt.Println(string(data))
				return nil
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tPROVIDER\tMOUNTS\tCREATED")
			for _, v := range resp.Volumes {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", v.Name, v.Provider, v.MountCount, v.CreatedAt)
			}
			tw.Flush()
			return nil
		},
	}
}

func newVolumeCreateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "create <name>",
		Short: "Create an empty named volume",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			client := newClient()

			var resp struct {
				Name string `json:"name"`
			}
			if err := client.Raw(cmd.Context(), "POST", "/api/v1/volumes", map[string]string{"name": name}, &resp); err != nil {
				return fmt.Errorf("create volume: %w", err)
			}

			fmt.Printf("Volume %q created\n", resp.Name)
			return nil
		},
	}
}

func newVolumeInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <name>",
		Short: "Show volume details: size, mounts, provider",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			client := newClient()

			var resp struct {
				Volume struct {
					Name       string `json:"name"`
					Provider   string `json:"provider"`
					MountCount int    `json:"mount_count"`
					CreatedAt  string `json:"created_at"`
					UpdatedAt  string `json:"updated_at"`
				} `json:"volume"`
				FileCount int   `json:"file_count"`
				TotalSize int64 `json:"total_size"`
			}
			path := fmt.Sprintf("/api/v1/volumes/%s", name)
			if err := client.Raw(cmd.Context(), "GET", path, nil, &resp); err != nil {
				return fmt.Errorf("inspect volume: %w", err)
			}

			if outputFmt == "json" {
				data, _ := json.MarshalIndent(resp, "", "  ")
				fmt.Println(string(data))
				return nil
			}

			fmt.Printf("Name:       %s\n", resp.Volume.Name)
			fmt.Printf("Provider:   %s\n", resp.Volume.Provider)
			fmt.Printf("Mounts:     %d\n", resp.Volume.MountCount)
			fmt.Printf("Files:      %d\n", resp.FileCount)
			fmt.Printf("Total Size: %s\n", formatBytes(resp.TotalSize))
			fmt.Printf("Created:    %s\n", resp.Volume.CreatedAt)
			fmt.Printf("Updated:    %s\n", resp.Volume.UpdatedAt)
			return nil
		},
	}
}

func newVolumeDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <name>",
		Short:   "Delete a volume (fails if mounted)",
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			client := newClient()

			path := fmt.Sprintf("/api/v1/volumes/%s", name)
			if err := client.Raw(cmd.Context(), "DELETE", path, nil, nil); err != nil {
				return fmt.Errorf("delete volume: %w", err)
			}

			fmt.Printf("Volume %q deleted\n", name)
			return nil
		},
	}
}

// formatBytes is defined in worker_cache.go
