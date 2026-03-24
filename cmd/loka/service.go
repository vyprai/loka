package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func newServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Manage LOKA services",
		Long: `Manage long-running services deployed to LOKA.

Examples:
  loka service list
  loka service get <id>
  loka service logs <id>
  loka service stop <id>
  loka service destroy <id>
  loka service redeploy <id>
  loka service env <id> --set KEY=VALUE`,
	}
	cmd.AddCommand(
		newServiceListCmd(),
		newServiceGetCmd(),
		newServiceLogsCmd(),
		newServiceStopCmd(),
		newServiceDestroyCmd(),
		newServiceRedeployCmd(),
		newServiceEnvCmd(),
	)
	return cmd
}

func newServiceListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List services",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			var resp struct {
				Services []struct {
					ID        string    `json:"ID"`
					Name      string    `json:"Name"`
					Status    string    `json:"Status"`
					Port      int       `json:"Port"`
					ImageRef  string    `json:"ImageRef"`
					Ready     bool      `json:"Ready"`
					CreatedAt time.Time `json:"CreatedAt"`
				} `json:"services"`
				Total int `json:"total"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/services", nil, &resp); err != nil {
				return err
			}
			if outputFmt == "json" {
				return printJSON(resp)
			}
			if len(resp.Services) == 0 {
				fmt.Println("No services. Deploy one: loka deploy .")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tSTATUS\tPORT\tIMAGE\tCREATED")
			for _, s := range resp.Services {
				status := s.Status
				if s.Ready {
					status = status + " (ready)"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
					shortID(s.ID), s.Name, status, s.Port,
					truncate(s.ImageRef, 30), s.CreatedAt.Format("2006-01-02 15:04"))
			}
			w.Flush()
			return nil
		},
	}
}

func newServiceGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <id>",
		Short: "Get service details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			var svc struct {
				ID            string            `json:"ID"`
				Name          string            `json:"Name"`
				Status        string            `json:"Status"`
				WorkerID      string            `json:"WorkerID"`
				ImageRef      string            `json:"ImageRef"`
				RecipeName    string            `json:"RecipeName"`
				Command       string            `json:"Command"`
				Args          []string          `json:"Args"`
				Env           map[string]string `json:"Env"`
				Port          int               `json:"Port"`
				VCPUs         int               `json:"VCPUs"`
				MemoryMB      int               `json:"MemoryMB"`
				BundleKey     string            `json:"BundleKey"`
				Ready         bool              `json:"Ready"`
				StatusMessage string            `json:"StatusMessage"`
				CreatedAt     time.Time         `json:"CreatedAt"`
				UpdatedAt     time.Time         `json:"UpdatedAt"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/services/"+args[0], nil, &svc); err != nil {
				return err
			}
			if outputFmt == "json" {
				return printJSON(svc)
			}
			fmt.Printf("ID:         %s\n", svc.ID)
			fmt.Printf("Name:       %s\n", svc.Name)
			fmt.Printf("Status:     %s\n", svc.Status)
			if svc.StatusMessage != "" {
				fmt.Printf("Message:    %s\n", svc.StatusMessage)
			}
			fmt.Printf("Ready:      %v\n", svc.Ready)
			fmt.Printf("Recipe:     %s\n", svc.RecipeName)
			fmt.Printf("Image:      %s\n", svc.ImageRef)
			fmt.Printf("Command:    %s %s\n", svc.Command, strings.Join(svc.Args, " "))
			fmt.Printf("Port:       %d\n", svc.Port)
			fmt.Printf("vCPUs:      %d\n", svc.VCPUs)
			fmt.Printf("Memory:     %d MB\n", svc.MemoryMB)
			fmt.Printf("Worker:     %s\n", svc.WorkerID)
			if svc.BundleKey != "" {
				fmt.Printf("Bundle:     %s\n", svc.BundleKey)
			}
			if len(svc.Env) > 0 {
				fmt.Printf("Env:\n")
				for k, v := range svc.Env {
					fmt.Printf("  %s=%s\n", k, v)
				}
			}
			fmt.Printf("Created:    %s\n", svc.CreatedAt.Format("2006-01-02 15:04:05"))
			fmt.Printf("Updated:    %s\n", svc.UpdatedAt.Format("2006-01-02 15:04:05"))
			return nil
		},
	}
}

func newServiceLogsCmd() *cobra.Command {
	var (
		follow bool
		lines  int
	)

	cmd := &cobra.Command{
		Use:   "logs <id>",
		Short: "View service logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()

			fetchLogs := func() error {
				var resp struct {
					Stdout []string `json:"stdout"`
					Stderr []string `json:"stderr"`
				}
				path := fmt.Sprintf("/api/v1/services/%s/logs?lines=%d", args[0], lines)
				if err := client.Raw(cmd.Context(), "GET", path, nil, &resp); err != nil {
					return err
				}
				for _, line := range resp.Stdout {
					fmt.Println(line)
				}
				for _, line := range resp.Stderr {
					fmt.Fprintf(os.Stderr, "%s\n", line)
				}
				return nil
			}

			if err := fetchLogs(); err != nil {
				return err
			}

			if follow {
				for {
					time.Sleep(2 * time.Second)
					if err := fetchLogs(); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow log output")
	cmd.Flags().IntVarP(&lines, "lines", "n", 100, "Number of lines to show")
	return cmd
}

func newServiceStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <id>",
		Short: "Stop a running service",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			var svc struct {
				ID     string `json:"ID"`
				Status string `json:"Status"`
			}
			if err := client.Raw(cmd.Context(), "POST", "/api/v1/services/"+args[0]+"/stop", nil, &svc); err != nil {
				return err
			}
			fmt.Printf("Service %s stopped\n", shortID(svc.ID))
			return nil
		},
	}
}

func newServiceDestroyCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "destroy <id>",
		Short:   "Destroy a service",
		Aliases: []string{"rm", "delete"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			if err := client.Raw(cmd.Context(), "DELETE", "/api/v1/services/"+args[0], nil, nil); err != nil {
				return err
			}
			fmt.Printf("Service %s destroyed\n", shortID(args[0]))
			return nil
		},
	}
}

func newServiceRedeployCmd() *cobra.Command {
	var wait bool

	cmd := &cobra.Command{
		Use:   "redeploy <id>",
		Short: "Redeploy a service",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			var svc struct {
				ID            string `json:"ID"`
				Status        string `json:"Status"`
				Ready         bool   `json:"Ready"`
				StatusMessage string `json:"StatusMessage"`
			}

			waitQuery := ""
			if wait {
				waitQuery = "?wait=true"
			}

			if err := client.Raw(cmd.Context(), "POST", "/api/v1/services/"+args[0]+"/redeploy"+waitQuery, nil, &svc); err != nil {
				return err
			}

			if !wait && !svc.Ready {
				fmt.Print("Redeploying...")
				for i := 0; i < 120; i++ {
					time.Sleep(1 * time.Second)
					var updated struct {
						Status string `json:"Status"`
						Ready  bool   `json:"Ready"`
					}
					if err := client.Raw(cmd.Context(), "GET", "/api/v1/services/"+svc.ID, nil, &updated); err != nil {
						break
					}
					if updated.Ready || updated.Status == "running" {
						break
					}
					if updated.Status == "error" {
						fmt.Println(" FAILED")
						return fmt.Errorf("redeploy failed")
					}
					fmt.Print(".")
				}
				fmt.Println(" ready!")
			}

			fmt.Printf("Service %s redeployed (status: %s)\n", shortID(svc.ID), svc.Status)
			return nil
		},
	}

	cmd.Flags().BoolVar(&wait, "wait", true, "Wait for service to be ready")
	return cmd
}

func newServiceEnvCmd() *cobra.Command {
	var (
		setVars   []string
		unsetVars []string
	)

	cmd := &cobra.Command{
		Use:   "env <id>",
		Short: "Get or set service environment variables",
		Long: `View or update environment variables for a service.

Examples:
  loka service env <id>                          # Show current env
  loka service env <id> --set KEY=VALUE          # Set a variable
  loka service env <id> --set A=1 --set B=2      # Set multiple
  loka service env <id> --unset KEY              # Remove a variable`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()

			if len(setVars) == 0 && len(unsetVars) == 0 {
				// Show current env.
				var svc struct {
					Env map[string]string `json:"Env"`
				}
				if err := client.Raw(cmd.Context(), "GET", "/api/v1/services/"+args[0], nil, &svc); err != nil {
					return err
				}
				if outputFmt == "json" {
					return printJSON(svc.Env)
				}
				if len(svc.Env) == 0 {
					fmt.Println("No environment variables set.")
					return nil
				}
				for k, v := range svc.Env {
					fmt.Printf("%s=%s\n", k, v)
				}
				return nil
			}

			// Get current env first.
			var svc struct {
				Env map[string]string `json:"Env"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/services/"+args[0], nil, &svc); err != nil {
				return err
			}
			env := svc.Env
			if env == nil {
				env = make(map[string]string)
			}

			// Apply sets.
			for _, kv := range setVars {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("invalid format %q, expected KEY=VALUE", kv)
				}
				env[k] = v
			}

			// Apply unsets.
			for _, k := range unsetVars {
				delete(env, k)
			}

			// Update.
			var updated struct {
				ID     string `json:"ID"`
				Status string `json:"Status"`
			}
			if err := client.Raw(cmd.Context(), "PUT", "/api/v1/services/"+args[0]+"/env", map[string]any{"env": env}, &updated); err != nil {
				return err
			}

			fmt.Printf("Environment updated for service %s\n", shortID(args[0]))
			return nil
		},
	}

	cmd.Flags().StringArrayVar(&setVars, "set", nil, "Set environment variable (KEY=VALUE, repeatable)")
	cmd.Flags().StringArrayVar(&unsetVars, "unset", nil, "Remove environment variable (repeatable)")
	return cmd
}

// truncate shortens a string to maxLen, appending "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
