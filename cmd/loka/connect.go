package main

import (
	"fmt"
	"time"

	"github.com/vyprai/loka/pkg/lokaapi"
	"github.com/spf13/cobra"
)

func newConnectCmd() *cobra.Command {
	var (
		name     string
		token    string
		caCert   string
		insecure bool
	)

	cmd := &cobra.Command{
		Use:   "connect <endpoint>",
		Short: "Connect to an existing LOKA server",
		Long: `Connect to a LOKA control plane that's already running — anywhere.

Examples:
  loka connect http://10.0.0.1:6840 --name prod
  loka connect https://loka.mycompany.com --name staging --token loka_abc123
  loka connect https://10.0.0.1:8443 --name secure --ca-cert ./server.crt
  loka connect https://10.0.0.1:8443 --name dev --insecure`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			endpoint := args[0]
			if name == "" {
				name = endpoint
			}

			// Build client with TLS options if needed.
			fmt.Printf("Connecting to %s...\n", endpoint)
			var client *lokaapi.Client
			if caCert != "" || insecure {
				c, err := lokaapi.NewClientWithTLS(endpoint, token, lokaapi.TLSOptions{
					CACertPath: caCert,
					Insecure:   insecure,
				})
				if err != nil {
					return fmt.Errorf("TLS setup failed: %w", err)
				}
				client = c
			} else {
				client = lokaapi.NewClient(endpoint, token)
			}

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

			// Save as a server.
			meta := map[string]string{
				"connected": "true",
			}
			if caCert != "" {
				meta["ca_cert"] = caCert
			}
			if insecure {
				meta["insecure"] = "true"
			}

			store, _ := loadDeployments()
			store.Add(Deployment{
				Name:      name,
				Provider:  "remote",
				Endpoint:  endpoint,
				Token:     token,
				Workers:   health.WorkersTotal,
				Status:    health.Status,
				CreatedAt: time.Now(),
				Meta:      meta,
			})
			store.Active = name
			saveDeployments(store)

			fmt.Printf("\nConnected. Server %q is now active.\n", name)
			fmt.Println()
			fmt.Println("Next:")
			fmt.Println("  loka status")
			fmt.Println("  loka image pull python:3.12-slim")
			fmt.Println("  loka session create --image python:3.12-slim")
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Server name (default: endpoint URL)")
	cmd.Flags().StringVarP(&token, "token", "t", "", "API token for authentication")
	cmd.Flags().StringVar(&caCert, "ca-cert", "", "Path to CA certificate for TLS verification")
	cmd.Flags().BoolVar(&insecure, "insecure", false, "Skip TLS certificate verification")
	return cmd
}
