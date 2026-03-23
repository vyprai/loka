package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newDeployVMCmd() *cobra.Command {
	var (
		name       string
		cp         string
		workers    []string
		sshUser    string
		sshKey     string
		cpPort     string
	)

	cmd := &cobra.Command{
		Use:   "vm",
		Short: "Deploy LOKA to your own VMs via SSH",
		Long: `Provide VM addresses and LOKA will SSH in, install, and configure everything.

Examples:
  # Single node (CP + worker on same machine)
  loka deploy vm --name dev --cp 10.0.0.1

  # Multi-node
  loka deploy vm --name prod \
    --cp 10.0.0.1 \
    --worker 10.0.0.2 \
    --worker 10.0.0.3 \
    --worker 10.0.0.4 \
    --ssh-user ubuntu \
    --ssh-key ~/.ssh/id_rsa`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cp == "" {
				return fmt.Errorf("--cp is required (control plane VM address)")
			}
			if name == "" {
				name = "vm"
			}
			if sshUser == "" {
				sshUser = "root"
			}
			if cpPort == "" {
				cpPort = "8080"
			}

			fmt.Printf("Deploying %q\n", name)
			fmt.Printf("  Control plane: %s\n", cp)
			if len(workers) == 0 {
				fmt.Printf("  Workers:       %s (same as CP)\n", cp)
			} else {
				fmt.Printf("  Workers:       %s\n", strings.Join(workers, ", "))
			}
			fmt.Printf("  SSH user:      %s\n", sshUser)
			fmt.Println()

			// Step 1: Install and start control plane.
			fmt.Printf("[1/3] Installing on control plane (%s)...\n", cp)
			if err := sshRun(cp, sshUser, sshKey, installScript()); err != nil {
				return fmt.Errorf("failed to install on CP: %w", err)
			}
			fmt.Printf("[1/3] Starting lokad on %s...\n", cp)
			if err := sshRun(cp, sshUser, sshKey, "nohup lokad > /var/log/lokad.log 2>&1 &"); err != nil {
				return fmt.Errorf("failed to start lokad: %w", err)
			}
			fmt.Printf("  Control plane running at http://%s:%s\n\n", cp, cpPort)

			// Step 2: Create worker token.
			fmt.Println("[2/3] Creating worker registration token...")
			tokenOut, err := sshOutput(cp, sshUser, sshKey,
				fmt.Sprintf("sleep 2 && curl -s -X POST http://localhost:%s/api/v1/worker-tokens -H 'Content-Type: application/json' -d '{\"name\":\"%s\",\"expires_seconds\":86400}'", cpPort, name))
			if err != nil {
				fmt.Printf("  Warning: could not create token automatically (%v)\n", err)
				fmt.Println("  Create one manually: loka token create --name worker")
			}
			// Extract token from JSON response.
			workerToken := extractToken(tokenOut)
			if workerToken != "" {
				fmt.Printf("  Token: %s...%s\n\n", workerToken[:12], workerToken[len(workerToken)-4:])
			}

			// Step 3: Install and start workers.
			workerHosts := workers
			if len(workerHosts) == 0 {
				workerHosts = []string{cp} // Single node — worker on same machine.
			}

			for i, w := range workerHosts {
				fmt.Printf("[3/3] Installing worker %d/%d (%s)...\n", i+1, len(workerHosts), w)
				if w != cp {
					// Install on worker node.
					if err := sshRun(w, sshUser, sshKey, installScript()); err != nil {
						fmt.Printf("  Warning: install failed on %s: %v\n", w, err)
						continue
					}
				}

				// Write worker config and start.
				workerConfig := fmt.Sprintf(`cat > /etc/loka/worker.yaml << 'WCFG'
control_plane:
  address: "%s:9090"
data_dir: /var/loka/worker
provider: selfmanaged
token: "%s"
labels:
  node: worker-%d
WCFG
nohup loka-worker > /var/log/loka-worker.log 2>&1 &`, cp, workerToken, i+1)

				if err := sshRun(w, sshUser, sshKey, workerConfig); err != nil {
					fmt.Printf("  Warning: failed to start worker on %s: %v\n", w, err)
					continue
				}
				fmt.Printf("  Worker %d running on %s\n", i+1, w)
			}

			fmt.Println()

			// Save deployment.
			endpoint := fmt.Sprintf("http://%s:%s", cp, cpPort)
			store, _ := loadDeployments()
			store.Add(Deployment{
				Name:      name,
				Provider:  "vm",
				Region:    cp,
				Endpoint:  endpoint,
				Workers:   len(workerHosts),
				Status:    "running",
				CreatedAt: time.Now(),
				Meta: map[string]string{
					"cp":       cp,
					"workers":  strings.Join(workerHosts, ","),
					"ssh_user": sshUser,
				},
			})
			store.Active = name
			saveDeployments(store)

			fmt.Printf("Deployment %q ready\n", name)
			fmt.Printf("  Endpoint: %s\n", endpoint)
			fmt.Printf("  Workers:  %d\n", len(workerHosts))
			fmt.Println()
			fmt.Println("Next:")
			fmt.Printf("  loka use %s\n", name)
			fmt.Println("  loka image pull python:3.12-slim")
			fmt.Println("  loka session create --image python:3.12-slim")
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Deployment name (default: vm)")
	cmd.Flags().StringVar(&cp, "cp", "", "Control plane VM address (required)")
	cmd.Flags().StringArrayVar(&workers, "worker", nil, "Worker VM addresses (repeatable, default: same as CP)")
	cmd.Flags().StringVar(&sshUser, "ssh-user", "root", "SSH username")
	cmd.Flags().StringVar(&sshKey, "ssh-key", "", "SSH private key path")
	cmd.Flags().StringVar(&cpPort, "port", "8080", "Control plane API port")

	return cmd
}

// ── SSH helpers ─────────────────────────────────────────

func sshRun(host, user, keyPath, command string) error {
	args := sshArgs(host, user, keyPath)
	args = append(args, command)
	cmd := exec.Command("ssh", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func sshOutput(host, user, keyPath, command string) (string, error) {
	args := sshArgs(host, user, keyPath)
	args = append(args, command)
	out, err := exec.Command("ssh", args...).Output()
	return string(out), err
}

func sshArgs(host, user, keyPath string) []string {
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
	}
	if keyPath != "" {
		args = append(args, "-i", keyPath)
	}
	args = append(args, fmt.Sprintf("%s@%s", user, host))
	return args
}

func installScript() string {
	return "curl -fsSL https://rizqme.github.io/loka/install.sh | bash"
}

func extractToken(jsonResp string) string {
	// Simple extraction — find "Token":"loka_..."
	idx := strings.Index(jsonResp, "loka_")
	if idx == -1 {
		return ""
	}
	end := strings.IndexAny(jsonResp[idx:], "\"}")
	if end == -1 {
		return jsonResp[idx:]
	}
	return jsonResp[idx : idx+end]
}
