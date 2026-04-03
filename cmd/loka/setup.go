package main

import (
	"context"
	crypto_tls "crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/vyprai/loka/internal/provider"
	awspkg "github.com/vyprai/loka/internal/provider/aws"
	azurepkg "github.com/vyprai/loka/internal/provider/azure"
	dopkg "github.com/vyprai/loka/internal/provider/digitalocean"
	gcppkg "github.com/vyprai/loka/internal/provider/gcp"
	ovhpkg "github.com/vyprai/loka/internal/provider/ovh"
	"github.com/vyprai/loka/pkg/lokaapi"
)

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
	fmt.Printf("  Stop:     loka space down\n")
	return nil
}

func deployLocalMacOS(name string, foreground bool) error {
	// Refuse to start if lokad is already running.
	if out, _ := exec.Command("pgrep", "-x", "lokad").Output(); len(out) > 0 {
		return fmt.Errorf("lokad is already running. Stop it first: loka space down")
	}

	// Find lokad binary — it embeds the Apple VZ hypervisor (lokavm library).
	lokadPath := findLokad()
	if lokadPath == "" {
		return fmt.Errorf("lokad binary not found. Run: make build")
	}

	home, _ := os.UserHomeDir()
	dataDir := filepath.Join(home, ".loka")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	if foreground {
		cmd := exec.Command(lokadPath, "--data-dir", dataDir)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	// Start lokad in background.
	fmt.Print("Starting LOKA...")
	logPath := filepath.Join(dataDir, "lokad.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("create log file: %w", err)
	}
	cmd := exec.Command(lokadPath, "--data-dir", dataDir)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start lokad: %w", err)
	}
	logFile.Close()

	// Wait for health — try HTTPS first (auto-TLS), then HTTP.
	fmt.Print(" waiting...")
	ready := false
	insecureClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &crypto_tls.Config{InsecureSkipVerify: true},
	}}
	for i := 0; i < 60; i++ {
		// Try HTTPS (lokad auto-generates TLS certs).
		resp, err := insecureClient.Get("https://localhost:6840/api/v1/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				ready = true
				break
			}
		}
		// Fall back to HTTP (if TLS disabled).
		resp, err = http.Get("http://localhost:6840/api/v1/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(1 * time.Second)
		fmt.Print(".")
	}
	if !ready {
		fmt.Println(" FAILED")
		logOut, _ := os.ReadFile(logPath)
		lines := strings.Split(string(logOut), "\n")
		if len(lines) > 10 {
			lines = lines[len(lines)-10:]
		}
		return fmt.Errorf("lokad did not become healthy:\n%s", strings.Join(lines, "\n"))
	}
	fmt.Println(" ready!")

	// Fetch CA cert from the server's /ca.crt endpoint.
	fmt.Print("  Fetching CA certificate...")
	caCertLocalPath := ""
	fetched, fetchErr := fetchCACert("https://localhost:6840")
	if fetchErr == nil && fetched != "" {
		caCertLocalPath = fetched
		fmt.Printf(" %s\n", caCertLocalPath)
	} else {
		fmt.Println(" not available")
	}

	// Save deployment.
	store, _ := loadDeployments()
	meta := map[string]string{
		"runtime": "lokad",
		"pid":     fmt.Sprint(cmd.Process.Pid),
	}
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

	fmt.Printf("LOKA started\n  Endpoint: https://localhost:6840\n  Stop: loka space down\n")

	// Hint about DNS if not enabled.
	if !isDNSEnabled() {
		fmt.Println()
		fmt.Printf("  %sTip:%s Enable .loka domains: %sloka dns enable%s\n", dim, reset, bold, reset)
	}

	return nil
}

// findLokad searches common locations for the lokad binary.
// Checks sibling directory first (for ./bin/lokad next to ./bin/loka).
func findLokad() string {
	if self, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(self), "lokad")
		if _, err := os.Stat(sibling); err == nil {
			return sibling
		}
	}
	candidates := []string{
		filepath.Join(os.Getenv("HOME"), ".loka", "bin", "lokad"),
		"lokad",
		"/usr/local/bin/lokad",
	}
	for _, p := range candidates {
		if path, err := exec.LookPath(p); err == nil {
			return path
		}
	}
	return ""
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
				// Kill lokad process and wait for it to exit.
				exec.Command("pkill", "-x", "lokad").Run()
				for i := 0; i < 10; i++ {
					if out, _ := exec.Command("pgrep", "-x", "lokad").Output(); len(out) == 0 {
						break
					}
					time.Sleep(500 * time.Millisecond)
				}
				// Force kill if still alive.
				if out, _ := exec.Command("pgrep", "-x", "lokad").Output(); len(out) > 0 {
					exec.Command("pkill", "-9", "-x", "lokad").Run()
					time.Sleep(500 * time.Millisecond)
				}
				fmt.Printf("LOKA %q stopped\n", name)
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
			fmt.Printf("Space:    %s\n", d.Provider)
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

func deployAWS(o deployOpts) error {
	if o.Region == "" { o.Region = "us-east-1" }
	fmt.Printf("Deploying %q to AWS (%s, %d workers)...\n", o.Name, o.Region, o.Workers)

	p, err := awspkg.New(awspkg.Config{
		Region:    o.Region,
		AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		KeyName:   o.SSHKey,
	}, slog.Default())
	if err != nil { return fmt.Errorf("AWS provider: %w", err) }

	workers, err := p.Provision(context.Background(), provider.ProvisionOpts{
		InstanceType: o.InstanceType, Region: o.Region, Count: o.Workers,
		SSHKeyName: o.SSHKey, Labels: map[string]string{"deployment": o.Name},
	})
	if err != nil { return err }
	for _, w := range workers {
		fmt.Printf("  Worker %s: %s (%s)\n", w.ID, w.ExternalIP, w.Status)
	}
	return nil
}

func deployGCP(o deployOpts) error {
	if o.Zone == "" { o.Zone = "us-central1-a" }
	fmt.Printf("Deploying %q to GCP (%s, %d workers)...\n", o.Name, o.Zone, o.Workers)

	p, err := gcppkg.New(gcppkg.Config{
		ProjectID: o.Project, Zone: o.Zone,
	}, slog.Default())
	if err != nil { return fmt.Errorf("GCP provider: %w", err) }

	workers, err := p.Provision(context.Background(), provider.ProvisionOpts{
		InstanceType: o.InstanceType, Zone: o.Zone, Count: o.Workers,
		Labels: map[string]string{"deployment": o.Name},
	})
	if err != nil { return err }
	for _, w := range workers {
		fmt.Printf("  Worker %s: %s (%s)\n", w.ID, w.ExternalIP, w.Status)
	}
	return nil
}

func deployAzure(o deployOpts) error {
	fmt.Printf("Deploying %q to Azure (%d workers)...\n", o.Name, o.Workers)

	p, err := azurepkg.New(azurepkg.Config{
		SubscriptionID: os.Getenv("AZURE_SUBSCRIPTION_ID"),
		ResourceGroup:  os.Getenv("AZURE_RESOURCE_GROUP"),
		Location:        o.Region,
	}, slog.Default())
	if err != nil { return fmt.Errorf("Azure provider: %w", err) }

	workers, err := p.Provision(context.Background(), provider.ProvisionOpts{
		InstanceType: o.InstanceType, Count: o.Workers,
		Labels: map[string]string{"deployment": o.Name},
	})
	if err != nil { return err }
	for _, w := range workers {
		fmt.Printf("  Worker %s (%s)\n", w.ID, w.Status)
	}
	return nil
}

func deployDigitalOcean(o deployOpts) error {
	if o.Region == "" { o.Region = "nyc1" }
	fmt.Printf("Deploying %q to DigitalOcean (%s, %d workers)...\n", o.Name, o.Region, o.Workers)

	token := os.Getenv("DIGITALOCEAN_TOKEN")
	if token == "" { return fmt.Errorf("DIGITALOCEAN_TOKEN environment variable is required") }

	p, err := dopkg.New(dopkg.Config{Token: token, Region: o.Region}, slog.Default())
	if err != nil { return fmt.Errorf("DigitalOcean provider: %w", err) }

	workers, err := p.Provision(context.Background(), provider.ProvisionOpts{
		InstanceType: o.InstanceType, Region: o.Region, Count: o.Workers,
		Labels: map[string]string{"deployment": o.Name},
	})
	if err != nil { return err }
	for _, w := range workers {
		fmt.Printf("  Droplet %s: %s (%s)\n", w.ID, w.ExternalIP, w.Status)
	}
	return nil
}

func deployOVH(o deployOpts) error {
	if o.Region == "" { o.Region = "GRA11" }
	fmt.Printf("Deploying %q to OVH (%s, %d workers)...\n", o.Name, o.Region, o.Workers)

	p, err := ovhpkg.New(ovhpkg.Config{
		ApplicationKey:    os.Getenv("OVH_APPLICATION_KEY"),
		ApplicationSecret: os.Getenv("OVH_APPLICATION_SECRET"),
		ConsumerKey:       os.Getenv("OVH_CONSUMER_KEY"),
		ProjectID:         os.Getenv("OVH_PROJECT_ID"),
		Region:            o.Region,
	}, slog.Default())
	if err != nil { return fmt.Errorf("OVH provider: %w", err) }

	workers, err := p.Provision(context.Background(), provider.ProvisionOpts{
		InstanceType: o.InstanceType, Region: o.Region, Count: o.Workers,
		Labels: map[string]string{"deployment": o.Name},
	})
	if err != nil { return err }
	for _, w := range workers {
		fmt.Printf("  Instance %s: %s (%s)\n", w.ID, w.ExternalIP, w.Status)
	}
	return nil
}

func findBinary(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil { return p, nil }
	for _, p := range []string{"./bin/" + name, "/usr/local/bin/" + name} { if _, err := os.Stat(p); err == nil { return p, nil } }
	return "", fmt.Errorf("%s not found", name)
}
