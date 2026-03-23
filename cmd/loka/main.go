package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/rizqme/loka/pkg/lokaapi"
	"github.com/rizqme/loka/pkg/version"
)

var (
	serverAddr string
	token      string
	outputFmt  string
)

func newClient() *lokaapi.Client {
	return lokaapi.NewClient(serverAddr, token)
}

func main() {
	rootCmd := &cobra.Command{
		Use:   "loka",
		Short: "LOKA — controlled execution environment for AI agents",
		Long:  "Deploy, manage, and interact with LOKA sessions, workers, and infrastructure.",
	}

	rootCmd.PersistentFlags().StringVarP(&serverAddr, "server", "s", "http://localhost:8080", "Control plane address")
	rootCmd.PersistentFlags().StringVarP(&token, "token", "t", "", "Auth token")
	rootCmd.PersistentFlags().StringVarP(&outputFmt, "output", "o", "table", "Output format: table, json")

	rootCmd.AddCommand(
		newVersionCmd(),
		newDeployCmd(),
		newSessionCmd(),
		newExecCmd(),
		newCheckpointCmd(),
		newWorkerCmd(),
		newProviderCmd(),
		newTokenCmd(),
		newStatusCmd(),
	)

	rootCmd.AddCommand(&cobra.Command{
		Use:   "completion [bash|zsh|fish]",
		Short: "Generate shell completion scripts",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return rootCmd.GenBashCompletion(os.Stdout)
			case "zsh":
				return rootCmd.GenZshCompletion(os.Stdout)
			case "fish":
				return rootCmd.GenFishCompletion(os.Stdout, true)
			default:
				return fmt.Errorf("unsupported shell: %s", args[0])
			}
		},
	})

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("loka %s (%s) built %s\n", version.Version, version.Commit, version.BuildTime)
		},
	}
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show overall system status",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()

			// Health.
			var health struct {
				Status       string `json:"status"`
				WorkersTotal int    `json:"workers_total"`
				WorkersReady int    `json:"workers_ready"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/health", nil, &health); err != nil {
				return fmt.Errorf("control plane unreachable: %w", err)
			}

			fmt.Printf("Control Plane:  %s\n", health.Status)
			fmt.Printf("Server:         %s\n", serverAddr)
			fmt.Printf("Workers:        %d ready / %d total\n", health.WorkersReady, health.WorkersTotal)

			// Sessions.
			sessions, err := client.ListSessions(cmd.Context())
			if err == nil {
				running, paused, terminated := 0, 0, 0
				for _, s := range sessions.Sessions {
					switch s.Status {
					case "running":
						running++
					case "paused":
						paused++
					case "terminated":
						terminated++
					}
				}
				fmt.Printf("Sessions:       %d running, %d paused, %d terminated\n", running, paused, terminated)
			}

			// Providers.
			var provResp struct {
				Providers []struct{ Name string } `json:"providers"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/providers", nil, &provResp); err == nil {
				fmt.Printf("Providers:      %d registered\n", len(provResp.Providers))
			}

			fmt.Printf("Metrics:        %s/metrics\n", serverAddr)
			return nil
		},
	}
}
