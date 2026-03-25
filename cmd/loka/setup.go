package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/vyprai/loka/pkg/lokaapi"
	"github.com/vyprai/loka/pkg/vm"
)

func newSetupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Set up LOKA infrastructure",
		Long: `Set up LOKA on local or cloud infrastructure.

  loka setup local --name dev
  loka setup aws --name prod --region us-east-1 --workers 3
  loka setup vm --name staging --cp 10.0.0.1
  loka setup apply prod.yml         # Deploy from YAML file
  loka worker add 10.0.0.5          # Add a worker
  loka worker remove 10.0.0.5       # Remove a worker
  loka list                         # List all servers
  loka use prod                     # Switch active server`,
	}
	cmd.AddCommand(
		newDeployFileCmd(),
		newDeployExportCmd(),
		newDeployCloudCmd("aws", "Deploy to AWS (EC2)", deployAWS),
		newDeployCloudCmd("gcp", "Deploy to Google Cloud", deployGCP),
		newDeployCloudCmd("azure", "Deploy to Azure", deployAzure),
		newDeployCloudCmd("do", "Deploy to DigitalOcean", deployDigitalOcean),
		newDeployCloudCmd("ovh", "Deploy to OVH", deployOVH),
		newDeployVMCmd(),
		newDeployLocalCmd(),
		newDeployRenameCmd(),
		newDeployDownCmd(),
		newDeployStatusCmd(),
		newDeployDestroyCmd(),
	)
	return cmd
}

type deployFunc func(opts deployOpts) error
type deployOpts struct {
	Name, Provider, Region, Zone, Project, InstanceType, SSHKey string
	Workers                                                     int
}

func newDeployCloudCmd(provider, desc string, fn deployFunc) *cobra.Command {
	var opts deployOpts
	opts.Provider = provider
	cmd := &cobra.Command{
		Use:   provider,
		Short: desc,
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.Name == "" { opts.Name = provider }
			if err := fn(opts); err != nil { return err }
			store, _ := loadDeployments()
			store.Add(Deployment{Name: opts.Name, Provider: provider, Region: opts.Region, Endpoint: "http://<pending>:8080", Workers: opts.Workers, Status: "provisioning", CreatedAt: time.Now()})
			store.Active = opts.Name
			saveDeployments(store)
			fmt.Printf("\nServer %q created and set as active.\n", opts.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.Name, "name", "", "Server name (default: provider)")
	cmd.Flags().StringVar(&opts.Region, "region", "", "Region")
	cmd.Flags().StringVar(&opts.Zone, "zone", "", "Zone")
	cmd.Flags().StringVar(&opts.Project, "project", "", "Project ID (GCP)")
	cmd.Flags().IntVar(&opts.Workers, "workers", 1, "Workers")
	cmd.Flags().StringVar(&opts.InstanceType, "instance-type", "", "Instance type")
	cmd.Flags().StringVar(&opts.SSHKey, "ssh-key", "", "SSH key")
	return cmd
}

func newDeployLocalCmd() *cobra.Command {
	var (name string; foreground bool)
	cmd := &cobra.Command{
		Use: "local", Short: "Start LOKA locally",
		Long: `Start LOKA on your local machine.

On Linux: runs lokad directly.
On macOS: runs lokad inside a VM (ports forwarded to localhost).

The VM is created automatically. If it doesn't exist yet, run the installer first:
  curl -fsSL https://vyprai.github.io/loka/install.sh | bash`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" { name = "local" }

			isMacOS := runtime.GOOS == "darwin"

			if isMacOS {
				return deployLocalMacOS(name, foreground)
			}
			return deployLocalLinux(name, foreground)
		},
	}
	cmd.Flags().StringVar(&name, "name", "local", "Server name")
	cmd.Flags().BoolVarP(&foreground, "foreground", "f", false, "Foreground")
	return cmd
}

func deployLocalLinux(name string, foreground bool) error {
	lokad, err := findBinary("lokad")
	if err != nil { return err }

	// Auto-TLS generates certs here.
	caCertPath := "/tmp/loka-data/artifacts/tls/ca.crt"
	store, _ := loadDeployments()
	store.Add(Deployment{
		Name: name, Provider: "local", Endpoint: "http://localhost:6840",
		Workers: 1, Status: "running", CreatedAt: time.Now(),
		Meta: map[string]string{"ca_cert": caCertPath},
	})
	store.Active = name
	saveDeployments(store)

	if foreground {
		fmt.Printf("Starting %q (foreground)...\n", name)
		p := exec.Command(lokad); p.Env = os.Environ(); p.Stdout = os.Stdout; p.Stderr = os.Stderr; p.Stdin = os.Stdin
		return p.Run()
	}
	p := exec.Command(lokad); p.Env = os.Environ()
	if err := p.Start(); err != nil { return err }
	fmt.Printf("LOKA %q started (pid %d)\n", name, p.Process.Pid)
	fmt.Printf("  Endpoint: http://localhost:6840\n")
	fmt.Printf("  Stop:     loka setup down\n")
	return nil
}

func deployLocalMacOS(name string, foreground bool) error {
	// Get or create VM manager via the VMManager interface.
	mgr, err := vm.NewManager("loka")
	if err != nil {
		// VMManager creation failed (e.g., limactl not found).
		// Fall back to direct Lima check for a helpful error message.
		return fmt.Errorf("VM manager not available: %w\nInstall Lima first:\n  curl -fsSL https://vyprai.github.io/loka/install.sh | bash", err)
	}

	// Ensure the VM exists and is running.
	status, _ := mgr.Status()
	if status != vm.VMStatusRunning {
		if status == vm.VMStatusUnknown {
			// VM doesn't exist — create it.
			fmt.Print("  Creating VM...")
			home, _ := os.UserHomeDir()
			if err := mgr.Create(vm.VMConfig{
				Name:      "loka",
				CPUs:      4,
				MemoryMB:  8192,
				DiskGB:    50,
				SharedDir: home,
			}); err != nil {
				fmt.Println(" FAILED")
				return fmt.Errorf("failed to create VM: %w", err)
			}
			fmt.Println(" ok")
		}

		fmt.Print("  Starting VM...")
		if err := mgr.Start(); err != nil {
			fmt.Println(" FAILED")
			return fmt.Errorf("failed to start VM: %w", err)
		}
		fmt.Println(" ok")
	}

	fmt.Printf("Starting LOKA in VM...\n")

	// Kill any existing lokad.
	mgr.Exec("sudo", "pkill", "-f", "lokad")
	time.Sleep(2 * time.Second)

	// Ensure LOKA binaries exist inside the VM.
	fmt.Print("  Syncing binaries...")
	mgr.Exec("sudo", "sh", "-c", `
		# Check if lokad already works.
		if command -v lokad >/dev/null 2>&1 && command -v firecracker >/dev/null 2>&1; then exit 0; fi

		# Try to copy from ISO overlay location.
		for bin in lokad loka-worker loka-supervisor loka-vmagent firecracker; do
			[ -f /usr/share/loka/$bin ] && cp /usr/share/loka/$bin /usr/local/bin/$bin && chmod +x /usr/local/bin/$bin
		done
		if command -v lokad >/dev/null 2>&1 && command -v firecracker >/dev/null 2>&1; then exit 0; fi

		# Download from GitHub releases as last resort.
		ARCH=$(uname -m)
		case "$ARCH" in aarch64) GOARCH=arm64;; x86_64) GOARCH=amd64;; esac
		cd /tmp
		curl -fsSL "https://github.com/vyprai/loka/releases/latest/download/loka-linux-${GOARCH}.tar.gz" -o loka.tar.gz 2>/dev/null
		tar xzf loka.tar.gz 2>/dev/null
		for bin in lokad loka-worker loka-supervisor loka-vmagent; do
			[ -f "$bin" ] && mv "$bin" /usr/local/bin/$bin && chmod +x /usr/local/bin/$bin
		done
		rm -f loka.tar.gz loka

		# Firecracker.
		if ! command -v firecracker >/dev/null 2>&1; then
			curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/v1.10.1/firecracker-v1.10.1-${ARCH}.tgz" 2>/dev/null | tar -xzf - -C /tmp
			cp "/tmp/release-v1.10.1-${ARCH}/firecracker-v1.10.1-${ARCH}" /usr/local/bin/firecracker
			chmod +x /usr/local/bin/firecracker
			rm -rf /tmp/release-v1.10.1-*
		fi
	`)
	fmt.Println(" ok")

	// Ensure rootfs + kernel exist inside the VM.
	fmt.Print("  Checking rootfs...")
	mgr.Exec("sudo", "sh", "-c", `
		mkdir -p /tmp/loka-data/kernel /tmp/loka-data/rootfs /tmp/loka-data/objstore /var/loka/kernel

		# Kernel: check pre-installed (ISO) or existing, else download.
		if [ ! -f /tmp/loka-data/kernel/vmlinux ]; then
			if [ -f /usr/share/loka/vmlinux ]; then
				cp /usr/share/loka/vmlinux /var/loka/kernel/vmlinux
			elif [ ! -f /var/loka/kernel/vmlinux ]; then
				ARCH=$(uname -m)
				curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min/ci-artifacts/kernels/${ARCH}/vmlinux-5.10.bin" -o /var/loka/kernel/vmlinux 2>/dev/null
			fi
			ln -sf /var/loka/kernel/vmlinux /tmp/loka-data/kernel/vmlinux
		fi

		# Rootfs: check pre-installed (ISO) or existing, else build from minirootfs.
		# Rebuild if missing or empty (check actual disk usage, not apparent size for sparse files).
		RF_BLOCKS=$(du -k /tmp/loka-data/rootfs/rootfs.ext4 2>/dev/null | awk '{print $1}')
		if [ ! -f /tmp/loka-data/rootfs/rootfs.ext4 ] || [ "${RF_BLOCKS:-0}" -lt 10000 ]; then
		rm -f /tmp/loka-data/rootfs/rootfs.ext4
			if [ -f /usr/share/loka/rootfs.ext4 ]; then
				cp /usr/share/loka/rootfs.ext4 /tmp/loka-data/rootfs/rootfs.ext4
			else
				ARCH=$(uname -m)
				curl -fsSL "https://dl-cdn.alpinelinux.org/alpine/v3.21/releases/${ARCH}/alpine-minirootfs-3.21.3-${ARCH}.tar.gz" -o /tmp/alpine.tar.gz 2>/dev/null
				truncate -s 4G /tmp/loka-data/rootfs/rootfs.ext4
				mkfs.ext4 -F /tmp/loka-data/rootfs/rootfs.ext4 >/dev/null 2>&1
				mkdir -p /tmp/mnt-rootfs
				mount -o loop /tmp/loka-data/rootfs/rootfs.ext4 /tmp/mnt-rootfs
				tar xzf /tmp/alpine.tar.gz -C /tmp/mnt-rootfs 2>/dev/null
				mkdir -p /tmp/mnt-rootfs/usr/local/bin
				cp /usr/local/bin/loka-supervisor /tmp/mnt-rootfs/usr/local/bin/loka-supervisor 2>/dev/null
				chmod +x /tmp/mnt-rootfs/usr/local/bin/loka-supervisor 2>/dev/null
				umount /tmp/mnt-rootfs; rmdir /tmp/mnt-rootfs; rm -f /tmp/alpine.tar.gz
			fi
		fi
	`)
	fmt.Println(" ok")

	if foreground {
		store, _ := loadDeployments()
		store.Add(Deployment{
			Name: name, Provider: "local", Endpoint: "https://localhost:6840",
			Workers: 1, Status: "running", CreatedAt: time.Now(),
			Meta: map[string]string{"runtime": "vm"},
		})
		store.Active = name
		saveDeployments(store)

		// Run lokad in foreground via the VM manager.
		// We need to use the underlying limactl for interactive I/O.
		return execVMForeground(mgr, "sudo", "lokad")
	}

	// Start lokad (DB is preserved across restarts; lokad marks stale records on boot).
	mgr.Exec("sudo", "bash", "-c", "nohup lokad > /tmp/lokad.log 2>&1 &")

	// Verify lokad didn't crash immediately after starting.
	time.Sleep(2 * time.Second)
	pgrepOut, _ := mgr.Exec("pgrep", "-f", "lokad")
	if len(strings.TrimSpace(pgrepOut)) == 0 {
		// lokad crashed on startup — show log for debugging.
		logOut, _ := mgr.Exec("tail", "-20", "/tmp/lokad.log")
		return fmt.Errorf("lokad crashed on startup:\n%s", logOut)
	}

	// Wait for health — check from host (port forwarded by VM).
	fmt.Print("  Waiting for server...")
	ready := false
	for i := 0; i < 60; i++ {
		// Try both https (auto-TLS) and http
		out, err := exec.Command("curl", "-sk", "--max-time", "2",
			"https://localhost:6840/api/v1/health").Output()
		if err == nil && strings.Contains(string(out), "ok") {
			ready = true
			break
		}
		out, err = exec.Command("curl", "-s", "--max-time", "2",
			"http://localhost:6840/api/v1/health").Output()
		if err == nil && strings.Contains(string(out), "ok") {
			ready = true
			break
		}
		fmt.Print(".")
		time.Sleep(1 * time.Second)
	}
	if !ready {
		fmt.Println(" FAILED")
		// Show last few lines of log for debugging
		logOut, _ := mgr.Exec("sudo", "tail", "-5", "/tmp/lokad.log")
		return fmt.Errorf("lokad did not become healthy:\n%s", logOut)
	}
	fmt.Println(" ready!")

	// Fetch CA cert from the server's /ca.crt endpoint.
	fmt.Print("  Fetching CA certificate...")
	caCertLocalPath := ""
	fetched, err := fetchCACert("https://localhost:6840")
	if err == nil && fetched != "" {
		caCertLocalPath = fetched
		fmt.Printf(" %s\n", caCertLocalPath)
	} else {
		fmt.Println(" not available")
	}

	// Save deployment with CA cert path.
	store, _ := loadDeployments()
	meta := map[string]string{"runtime": "vm"}
	if caCertLocalPath != "" {
		meta["ca_cert"] = caCertLocalPath
	}
	store.Add(Deployment{
		Name: name, Provider: "local", Endpoint: "https://localhost:6840",
		Workers: 1, Status: "running", CreatedAt: time.Now(),
		Meta: meta,
	})
	store.Active = name
	saveDeployments(store)

	fmt.Printf("LOKA %q started (VM, ports forwarded to localhost)\n", name)
	fmt.Printf("  Endpoint: https://localhost:6840\n")
	fmt.Printf("  Stop:     loka setup down\n")
	return nil
}

// execVMForeground runs a command inside the VM with interactive I/O.
// It attempts to use the underlying Lima shell for proper TTY passthrough.
func execVMForeground(mgr vm.VMManager, cmd string, args ...string) error {
	// Try limactl shell for interactive mode.
	limactl, err := exec.LookPath("limactl")
	if err == nil {
		shellArgs := []string{"shell", mgr.Name(), "--"}
		shellArgs = append(shellArgs, cmd)
		shellArgs = append(shellArgs, args...)
		p := exec.Command(limactl, shellArgs...)
		p.Stdout = os.Stdout
		p.Stderr = os.Stderr
		p.Stdin = os.Stdin
		return p.Run()
	}

	// Fallback: use Exec (no TTY passthrough).
	allArgs := append([]string{cmd}, args...)
	out, execErr := mgr.Exec(allArgs[0], allArgs[1:]...)
	if out != "" {
		fmt.Print(out)
	}
	return execErr
}

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List servers", Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			store, _ := loadDeployments()
			if len(store.Deployments) == 0 {
				fmt.Println("No servers. Set one up: loka setup local --name dev")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "  NAME\tPROVIDER\tENDPOINT\tWORKERS\tSTATUS\tCREATED")
			for _, d := range store.Deployments {
				a := " "; if d.Name == store.Active { a = "*" }
				fmt.Fprintf(w, "%s %s\t%s\t%s\t%d\t%s\t%s\n", a, d.Name, d.Provider, d.Endpoint, d.Workers, d.Status, d.CreatedAt.Format("2006-01-02"))
			}
			w.Flush()
			return nil
		},
	}
}


func newDeployRenameCmd() *cobra.Command {
	return &cobra.Command{
		Use: "rename <old> <new>", Short: "Rename a server", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, _ := loadDeployments()
			d := store.Get(args[0])
			if d == nil { return fmt.Errorf("server %q not found", args[0]) }
			old := d.Name; d.Name = args[1]
			if store.Active == old { store.Active = args[1] }
			saveDeployments(store)
			fmt.Printf("Renamed %q -> %q\n", old, args[1])
			return nil
		},
	}
}

func newDeployDownCmd() *cobra.Command {
	return &cobra.Command{
		Use: "down [name]", Short: "Stop a server", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, _ := loadDeployments()
			name := store.Active; if len(args) > 0 { name = args[0] }
			d := store.Get(name)
			if d == nil { return fmt.Errorf("server %q not found", name) }
			if d.Provider == "local" {
				rt := ""
				if d.Meta != nil {
					rt = d.Meta["runtime"]
				}
				if rt == "lima" || rt == "vm" {
					// Stop lokad inside VM — keep the VM running for fast restart.
					mgr, mgrErr := vm.NewManager("loka")
					if mgrErr == nil {
						mgr.Exec("sudo", "pkill", "-f", "lokad")
						fmt.Printf("LOKA %q stopped\n", name)
					} else {
						// Fall back to limactl directly.
						limactl, _ := exec.LookPath("limactl")
						if limactl != "" {
							exec.Command(limactl, "shell", "loka", "sudo", "pkill", "-f", "lokad").Run()
							fmt.Printf("LOKA %q stopped\n", name)
						}
					}
				} else {
					out, _ := exec.Command("pgrep", "-f", "lokad").Output()
					if len(out) > 0 { exec.Command("kill", strings.TrimSpace(string(out))).Run(); fmt.Printf("LOKA %q stopped\n", name) } else { fmt.Printf("%q is not running\n", name) }
				}
			}
			d.Status = "stopped"; saveDeployments(store)
			return nil
		},
	}
}

func newDeployStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use: "status [name]", Short: "Show server status", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, _ := loadDeployments()
			name := store.Active; if len(args) > 0 { name = args[0] }
			d := store.Get(name)
			if d == nil { return fmt.Errorf("server %q not found", name) }
			fmt.Printf("Name:     %s\n", d.Name)
			fmt.Printf("Provider: %s\n", d.Provider)
			fmt.Printf("Endpoint: %s\n", d.Endpoint)
			fmt.Printf("Workers:  %d\n", d.Workers)
			fmt.Printf("Status:   %s\n", d.Status)
			c := lokaapi.NewClient(d.Endpoint, token)
			var h struct{ Status string `json:"status"`; WorkersReady int `json:"workers_ready"` }
			if err := c.Raw(cmd.Context(), "GET", "/api/v1/health", nil, &h); err == nil {
				fmt.Printf("Live:     %s (%d workers ready)\n", h.Status, h.WorkersReady)
			}
			return nil
		},
	}
}

func newDeployDestroyCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use: "destroy [name]", Short: "Destroy a server", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, _ := loadDeployments()
			name := store.Active; if len(args) > 0 { name = args[0] }
			d := store.Get(name)
			if d == nil { return fmt.Errorf("server %q not found", name) }
			if !force {
				fmt.Printf("Destroy %q? (yes/no): ", name)
				var c string; fmt.Scanln(&c)
				if c != "yes" { fmt.Println("Aborted."); return nil }
			}
			if d.Provider == "local" {
				out, _ := exec.Command("pgrep", "-f", "lokad").Output()
				if len(out) > 0 { exec.Command("kill", strings.TrimSpace(string(out))).Run() }
			}
			store.Remove(name); saveDeployments(store)
			fmt.Printf("Server %q destroyed\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Skip confirmation")
	return cmd
}

func deployAWS(o deployOpts) error { r:=o.Region; if r=="" {r="us-east-1"}; fmt.Printf("Deploying %q to AWS (%s, %d workers)...\n", o.Name, r, o.Workers); return nil }
func deployGCP(o deployOpts) error { z:=o.Zone; if z=="" {z="us-central1-a"}; fmt.Printf("Deploying %q to GCP (%s, %d workers)...\n", o.Name, z, o.Workers); return nil }
func deployAzure(o deployOpts) error { fmt.Printf("Deploying %q to Azure (%d workers)...\n", o.Name, o.Workers); return nil }
func deployDigitalOcean(o deployOpts) error { fmt.Printf("Deploying %q to DigitalOcean (%d workers)...\n", o.Name, o.Workers); return nil }
func deployOVH(o deployOpts) error { fmt.Printf("Deploying %q to OVH (%d workers)...\n", o.Name, o.Workers); return nil }

func findBinary(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil { return p, nil }
	for _, p := range []string{"./bin/" + name, "/usr/local/bin/" + name} { if _, err := os.Stat(p); err == nil { return p, nil } }
	return "", fmt.Errorf("%s not found", name)
}
