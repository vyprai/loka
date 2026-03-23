package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newAdminCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Administrative commands",
	}
	cmd.AddCommand(newAdminGCCmd(), newAdminRetentionCmd())
	return cmd
}

func newAdminGCCmd() *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Trigger garbage collection sweep",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			path := "/api/v1/admin/gc"
			if dryRun {
				path += "?dry_run=true"
			}
			var resp map[string]any
			if err := client.Raw(cmd.Context(), "POST", path, nil, &resp); err != nil {
				return err
			}
			if outputFmt == "json" {
				return printJSON(resp)
			}
			fmt.Println("GC sweep triggered.")
			if status, ok := resp["status"]; ok {
				fmt.Printf("  Status: %v\n", status)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview what would be collected without deleting")
	return cmd
}

func newAdminRetentionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "retention",
		Short: "Show retention configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			var resp map[string]any
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/admin/retention", nil, &resp); err != nil {
				return err
			}
			if outputFmt == "json" {
				return printJSON(resp)
			}
			fmt.Println("Retention Configuration:")
			for k, v := range resp {
				fmt.Printf("  %-20s %v\n", k+":", v)
			}
			return nil
		},
	}
}
