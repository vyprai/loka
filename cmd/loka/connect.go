package main

import (
	"fmt"
	"time"

	"github.com/rizqme/loka/pkg/lokaapi"
	"github.com/spf13/cobra"
)

func newConnectCmd() *cobra.Command {
	var (
		name  string
		token string
	)

	cmd := &cobra.Command{
		Use:   "connect <endpoint>",
		Short: "Connect to an existing LOKA server",
		Long: `Connect to a LOKA control plane that's already running — anywhere.

Examples:
  loka connect http://10.0.0.1:8080 --name prod
  loka connect https://loka.mycompany.com --name staging --token loka_abc123
  loka connect http://localhost:8080 --name local`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			endpoint := args[0]
			if name == "" {
				name = endpoint
			}

			// Verify the endpoint is reachable.
			fmt.Printf("Connecting to %s...\n", endpoint)
			client := lokaapi.NewClient(endpoint, token)
			var health struct {
				Status       string `json:"status"`
				WorkersTotal int    `json:"workers_total"`
				WorkersReady int    `json:"workers_ready"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/health", nil, &health); err != nil {
				return fmt.Errorf("cannot reach %s: %w", endpoint, err)
			}

			fmt.Printf("  Status:  %s\n", health.Status)
			fmt.Printf("  Workers: %d ready / %d total\n", health.WorkersReady, health.WorkersTotal)

			// Save as a deployment.
			store, _ := loadDeployments()
			store.Add(Deployment{
				Name:      name,
				Provider:  "remote",
				Endpoint:  endpoint,
				Token:     token,
				Workers:   health.WorkersTotal,
				Status:    health.Status,
				CreatedAt: time.Now(),
				Meta: map[string]string{
					"connected": "true",
				},
			})
			store.Active = name
			saveDeployments(store)

			fmt.Printf("\nConnected. Deployment %q is now active.\n", name)
			fmt.Println()
			fmt.Println("Next:")
			fmt.Println("  loka status")
			fmt.Println("  loka image pull python:3.12-slim")
			fmt.Println("  loka session create --image python:3.12-slim")
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Deployment name (default: endpoint URL)")
	cmd.Flags().StringVarP(&token, "token", "t", "", "API token for authentication")
	return cmd
}
