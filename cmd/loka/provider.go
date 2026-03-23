package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newProviderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "provider",
		Short: "Manage cloud providers",
	}
	cmd.AddCommand(
		newProviderListCmd(),
		newProviderProvisionCmd(),
		newProviderStatusCmd(),
	)
	return cmd
}

func newProviderListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered providers",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			var resp struct {
				Providers []struct {
					Name string `json:"name"`
				} `json:"providers"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/providers", nil, &resp); err != nil {
				return err
			}
			if outputFmt == "json" {
				return printJSON(resp)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "PROVIDER")
			for _, p := range resp.Providers {
				fmt.Fprintln(w, p.Name)
			}
			w.Flush()
			return nil
		},
	}
}

func newProviderProvisionCmd() *cobra.Command {
	var (
		instanceType string
		region       string
		count        int
	)
	cmd := &cobra.Command{
		Use:   "provision <provider>",
		Short: "Provision workers via a cloud provider",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			var resp struct {
				Workers []struct {
					ID       string `json:"ID"`
					Provider string `json:"Provider"`
					Region   string `json:"Region"`
					Status   int    `json:"Status"`
				} `json:"workers"`
			}
			err := client.Raw(cmd.Context(), "POST", "/api/v1/providers/"+args[0]+"/provision", map[string]any{
				"instance_type": instanceType,
				"region":        region,
				"count":         count,
			}, &resp)
			if err != nil {
				return err
			}
			for _, w := range resp.Workers {
				fmt.Printf("Provisioning worker %s (%s, %s)\n", w.ID, w.Provider, w.Region)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&instanceType, "instance-type", "", "Instance type (e.g., i3.metal)")
	cmd.Flags().StringVar(&region, "region", "", "Region")
	cmd.Flags().IntVar(&count, "count", 1, "Number of workers")
	return cmd
}

func newProviderStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <provider>",
		Short: "Show provider worker status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			var resp struct {
				Provider string `json:"provider"`
				Total    int    `json:"total"`
				Workers  []struct {
					ID     string `json:"ID"`
					Region string `json:"Region"`
					Status int    `json:"Status"`
				} `json:"workers"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/providers/"+args[0]+"/status", nil, &resp); err != nil {
				return err
			}
			if outputFmt == "json" {
				return printJSON(resp)
			}
			fmt.Printf("Provider: %s\n", resp.Provider)
			fmt.Printf("Workers:  %d\n", resp.Total)
			return nil
		},
	}
}
