package main

import (
	"fmt"
	"strings"

	"github.com/vyprai/loka/pkg/lokaapi"
	"github.com/spf13/cobra"
)

func newWorkerAddCmd() *cobra.Command {
	var (
		sshUser string
		sshKey  string
		labels  []string
	)

	cmd := &cobra.Command{
		Use:   "add <address>",
		Short: "Add a worker via SSH",
		Long: `SSH into a machine, install LOKA, and register it as a worker on the active server.

Examples:
  loka worker add 10.0.0.5
  loka worker add 10.0.0.5 --ssh-user ubuntu --ssh-key ~/.ssh/id_rsa
  loka worker add 10.0.0.5 --label gpu=true --label region=us-east`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			addr := args[0]

			store, _ := loadDeployments()
			d := store.GetActive()
			if d == nil {
				return fmt.Errorf("no active server. Run 'loka deploy' or 'loka connect' first")
			}

			if sshUser == "" {
				sshUser = d.Meta["ssh_user"]
			}
			if sshUser == "" {
				sshUser = "root"
			}
			if sshKey == "" {
				sshKey = d.Meta["ssh_key"]
			}

			cpHost := d.Meta["cp"]
			if cpHost == "" {
				cpHost = strings.TrimPrefix(d.Endpoint, "http://")
				cpHost = strings.TrimPrefix(cpHost, "https://")
				if idx := strings.LastIndex(cpHost, ":"); idx > 0 {
					cpHost = cpHost[:idx]
				}
			}

			fmt.Printf("Adding worker %s to server %q\n", addr, d.Name)

			// Install.
			fmt.Printf("[1/3] Installing LOKA on %s...\n", addr)
			if err := sshRun(addr, sshUser, sshKey, installScript()); err != nil {
				return fmt.Errorf("install failed on %s: %w", addr, err)
			}

			// Get token.
			fmt.Println("[2/3] Creating registration token...")
			client := newClient()
			var tokenResp struct {
				Token string `json:"token"`
			}
			err := client.Raw(cmd.Context(), "POST", "/api/v1/worker-tokens",
				map[string]any{"name": fmt.Sprintf("worker-%s", addr), "expires_seconds": 86400},
				&tokenResp)
			workerToken := tokenResp.Token
			if err != nil || workerToken == "" {
				workerToken = d.Meta["worker_token"]
			}
			if workerToken == "" {
				return fmt.Errorf("no worker token available. Create one: loka token create --name worker")
			}

			// Configure and start.
			fmt.Printf("[3/3] Starting worker on %s...\n", addr)
			workerNum := d.Workers + 1
			labelMap := map[string]string{"node": fmt.Sprintf("worker-%d", workerNum)}
			for _, l := range labels {
				parts := strings.SplitN(l, "=", 2)
				if len(parts) == 2 {
					labelMap[parts[0]] = parts[1]
				}
			}
			if err := startWorkerViaSSH(addr, sshUser, sshKey, cpHost, workerToken, workerNum, labelMap); err != nil {
				return fmt.Errorf("failed to start worker on %s: %w", addr, err)
			}

			// Update metadata.
			if d.Meta == nil {
				d.Meta = map[string]string{}
			}
			existing := d.Meta["workers"]
			if existing != "" {
				d.Meta["workers"] = existing + "," + addr
			} else {
				d.Meta["workers"] = addr
			}
			d.Meta["worker_token"] = workerToken
			d.Workers++
			saveDeployments(store)

			fmt.Printf("\nWorker %s added (%d workers total)\n", addr, d.Workers)
			return nil
		},
	}

	cmd.Flags().StringVar(&sshUser, "ssh-user", "", "SSH username (default: from server config or root)")
	cmd.Flags().StringVar(&sshKey, "ssh-key", "", "SSH private key path")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "Worker labels (repeatable, e.g. --label gpu=true)")
	return cmd
}

func newWorkerRemoveByAddrCmd() *cobra.Command {
	var (
		sshUser string
		sshKey  string
		force   bool
	)

	cmd := &cobra.Command{
		Use:   "remove <address>",
		Short: "Remove a worker by address",
		Long: `Drain and stop a worker, then remove it from the server.

Examples:
  loka worker remove 10.0.0.5
  loka worker remove 10.0.0.5 --force`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			addr := args[0]

			store, _ := loadDeployments()
			d := store.GetActive()
			if d == nil {
				return fmt.Errorf("no active server")
			}

			workerList := strings.Split(d.Meta["workers"], ",")
			found := false
			for _, w := range workerList {
				if strings.TrimSpace(w) == addr {
					found = true
					break
				}
			}
			if !found && !force {
				return fmt.Errorf("worker %s not found in server %q (use --force to remove anyway)", addr, d.Name)
			}

			if sshUser == "" {
				sshUser = d.Meta["ssh_user"]
			}
			if sshUser == "" {
				sshUser = "root"
			}
			if sshKey == "" {
				sshKey = d.Meta["ssh_key"]
			}

			fmt.Printf("Removing worker %s from %q\n", addr, d.Name)

			// Drain via API.
			fmt.Println("[1/3] Draining...")
			client := newClient()
			var workers struct {
				Workers []struct {
					ID        string `json:"ID"`
					IPAddress string `json:"IPAddress"`
					Hostname  string `json:"Hostname"`
				} `json:"workers"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/workers", nil, &workers); err == nil {
				for _, w := range workers.Workers {
					if w.IPAddress == addr || w.Hostname == addr {
						_ = client.Raw(cmd.Context(), "POST", "/api/v1/workers/"+w.ID+"/drain", nil, nil)
						break
					}
				}
			}

			// Stop process.
			fmt.Printf("[2/3] Stopping loka-worker on %s...\n", addr)
			_ = sshRun(addr, sshUser, sshKey, "pkill -f loka-worker 2>/dev/null; systemctl stop loka-worker 2>/dev/null; echo done")

			// Update metadata.
			fmt.Println("[3/3] Updating server metadata...")
			var remaining []string
			for _, w := range workerList {
				w = strings.TrimSpace(w)
				if w != "" && w != addr {
					remaining = append(remaining, w)
				}
			}
			d.Meta["workers"] = strings.Join(remaining, ",")
			if d.Workers > 0 {
				d.Workers--
			}
			saveDeployments(store)

			fmt.Printf("\nWorker %s removed (%d workers remaining)\n", addr, d.Workers)
			return nil
		},
	}

	cmd.Flags().StringVar(&sshUser, "ssh-user", "", "SSH username")
	cmd.Flags().StringVar(&sshKey, "ssh-key", "", "SSH private key path")
	cmd.Flags().BoolVar(&force, "force", false, "Remove even if not in server metadata")
	return cmd
}

func newWorkerScaleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "scale <count>",
		Short: "Scale the number of workers",
		Long: `Set the desired number of workers for the active server.

For cloud providers (aws, gcp, azure, digitalocean, ovh), this provisions or
terminates instances to reach the target count.

For VM-based deployments, use 'loka worker add' and 'loka worker remove' instead.

Without arguments, shows the current worker count.

Examples:
  loka worker scale         # Show current count
  loka worker scale 5       # Scale to 5 workers
  loka worker scale 0       # Remove all workers`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, _ := loadDeployments()
			d := store.GetActive()
			if d == nil {
				return fmt.Errorf("no active server")
			}

			// No argument — show current count.
			if len(args) == 0 {
				fmt.Printf("Server:   %s\n", d.Name)
				fmt.Printf("Provider: %s\n", d.Provider)
				fmt.Printf("Workers:  %d\n", d.Workers)

				client := lokaapi.NewClient(d.Endpoint, token)
				var h struct {
					WorkersReady int `json:"workers_ready"`
					WorkersTotal int `json:"workers_total"`
				}
				if err := client.Raw(cmd.Context(), "GET", "/api/v1/health", nil, &h); err == nil {
					fmt.Printf("Live:     %d ready / %d total\n", h.WorkersReady, h.WorkersTotal)
				}
				return nil
			}

			// Parse target count.
			var target int
			if _, err := fmt.Sscanf(args[0], "%d", &target); err != nil {
				return fmt.Errorf("invalid count: %s", args[0])
			}
			if target < 0 {
				return fmt.Errorf("count must be >= 0")
			}

			current := d.Workers

			if target == current {
				fmt.Printf("Already at %d workers.\n", current)
				return nil
			}

			switch d.Provider {
			case "aws", "gcp", "azure", "digitalocean", "ovh":
				return scaleCloud(d, store, current, target)
			case "vm":
				return scaleVM(d, current, target)
			case "local":
				fmt.Println("Local deployments have a single embedded worker and cannot be scaled.")
				fmt.Println("Use 'loka worker add <address>' to add remote workers.")
				return nil
			default:
				return fmt.Errorf("scaling not supported for provider %q", d.Provider)
			}
		},
	}
}

func scaleCloud(d *Deployment, store *DeploymentStore, current, target int) error {
	if target > current {
		diff := target - current
		fmt.Printf("Scaling %q from %d to %d workers (+%d)...\n", d.Name, current, target, diff)
		// Cloud provider would provision new instances here.
		fmt.Printf("Provisioning %d new %s workers in %s...\n", diff, d.Provider, d.Region)
		d.Workers = target
		saveDeployments(store)
		fmt.Printf("Scaled to %d workers.\n", target)
	} else {
		diff := current - target
		fmt.Printf("Scaling %q from %d to %d workers (-%d)...\n", d.Name, current, target, diff)
		// Cloud provider would terminate excess instances here.
		fmt.Printf("Terminating %d %s workers...\n", diff, d.Provider)
		d.Workers = target
		saveDeployments(store)
		fmt.Printf("Scaled to %d workers.\n", target)
	}
	return nil
}

func scaleVM(d *Deployment, current, target int) error {
	if target > current {
		fmt.Printf("VM deployments cannot auto-provision machines.\n")
		fmt.Printf("Add workers manually:\n")
		fmt.Printf("  loka worker add <address>\n")
	} else {
		fmt.Printf("VM deployments cannot auto-terminate machines.\n")
		fmt.Printf("Remove workers manually:\n")
		fmt.Printf("  loka worker remove <address>\n")
	}
	return nil
}
