package main

import (
	"fmt"
	"net/url"
	"os"

	"github.com/spf13/cobra"
	"github.com/vyprai/loka/pkg/lokaapi"
	"github.com/vyprai/loka/pkg/version"
)

var (
	serverAddr string
	token      string
	outputFmt  string
)

// resolveServer returns the endpoint, token, and TLS metadata for the active server.
func resolveServer() (endpoint, tok, caCert string, insecureTLS bool) {
	endpoint = serverAddr
	tok = token
	if endpoint == "http://localhost:6840" {
		store, err := loadDeployments()
		if err == nil {
			if d := store.GetActive(); d != nil {
				endpoint = d.Endpoint
				if tok == "" && d.Token != "" {
					tok = d.Token
				}
				if d.Meta != nil {
					caCert = d.Meta["ca_cert"]
					insecureTLS = d.Meta["insecure"] == "true"
				}
			}
		}
	}
	// TLS is determined by the deployment store metadata (ca_cert, insecure).
	// No auto-detection — use loka connect or loka deploy to configure TLS.
	return
}

// grpcAddr derives the gRPC address from an HTTP endpoint.
// http://host:6840 → host:6841, https://host:6843 → host:6841
func grpcAddr(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "localhost:6841"
	}
	host := u.Hostname()
	if host == "" {
		host = "localhost"
	}
	return host + ":6841"
}

// newGRPCClient creates a gRPC client targeting the active server.
// Tries TLS first. Falls back to plaintext with a warning.
func newGRPCClient() *lokaapi.GRPCClient {
	endpoint, tok, caCert, insecureTLS := resolveServer()
	addr := grpcAddr(endpoint)

	// Try TLS first if we have a CA cert.
	if caCert != "" {
		c, err := lokaapi.NewGRPCClient(lokaapi.GRPCOpts{
			Address:    addr,
			Token:      tok,
			CACertPath: caCert,
		})
		if err == nil {
			return c
		}
	}

	// Try insecure TLS.
	if insecureTLS {
		c, err := lokaapi.NewGRPCClient(lokaapi.GRPCOpts{
			Address:  addr,
			Token:    tok,
			Insecure: true,
		})
		if err == nil {
			return c
		}
	}

	// Fall back to plaintext — warn.
	c, err := lokaapi.NewGRPCClient(lokaapi.GRPCOpts{
		Address:   addr,
		Token:     tok,
		PlainText: true,
	})
	if err != nil {
		return nil
	}
	fmt.Fprintf(os.Stderr, "warning: gRPC connection to %s is not encrypted\n", addr)
	return c
}

// newClient creates an HTTP REST client (fallback).
func newClient() *lokaapi.Client {
	endpoint, tok, caCert, insecureTLS := resolveServer()
	if caCert != "" || insecureTLS {
		c, err := lokaapi.NewClientWithTLS(endpoint, tok, lokaapi.TLSOptions{
			CACertPath: caCert,
			Insecure:   insecureTLS,
		})
		if err == nil {
			return c
		}
	}
	return lokaapi.NewClient(endpoint, tok)
}

func main() {
	rootCmd := &cobra.Command{
		Use:           "loka",
		Short:         "LOKA — controlled execution environment for AI agents",
		Long:          "Deploy, manage, and interact with LOKA sessions, workers, and infrastructure.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.PersistentFlags().StringVarP(&serverAddr, "server", "s", "http://localhost:6840", "Control plane address")
	rootCmd.PersistentFlags().StringVarP(&token, "token", "t", "", "Auth token")
	rootCmd.PersistentFlags().StringVarP(&outputFmt, "output", "o", "table", "Output format: table, json")

	rootCmd.AddCommand(
		newVersionCmd(),
		newListCmd(),
		newCurrentCmd(),
		newConnectCmd(),
		newDeployCmd(),
		newUseCmd(),
		newSessionCmd(),
		newExecCmd(),
		newShellCmd(),
		newCheckpointCmd(),
		newWorkerCmd(),
		newProviderCmd(),
		newTokenCmd(),
		newStatusCmd(),
		newDomainsCmd(),
		newAdminCmd(),
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
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
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
