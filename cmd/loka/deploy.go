package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rizqme/loka/pkg/lokaapi"
	"github.com/spf13/cobra"
)

func newDeployCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Manage LOKA deployments",
		Long: `Create, list, switch, and destroy LOKA deployments.

  loka deploy local --name dev
  loka deploy aws --name prod --region us-east-1 --workers 3
  loka deploy list
  loka deploy use prod
  loka deploy status
  loka deploy down
  loka deploy destroy prod`,
	}
	cmd.AddCommand(
		newDeployCloudCmd("aws", "Deploy to AWS (EC2)", deployAWS),
		newDeployCloudCmd("gcp", "Deploy to Google Cloud", deployGCP),
		newDeployCloudCmd("azure", "Deploy to Azure", deployAzure),
		newDeployCloudCmd("digitalocean", "Deploy to DigitalOcean", deployDigitalOcean),
		newDeployCloudCmd("ovh", "Deploy to OVH", deployOVH),
		newDeployVMCmd(),
		newDeployLocalCmd(),
		newDeployListCmd(),
		newDeployUseCmd(),
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
			fmt.Printf("\nDeployment %q created and set as active.\n", opts.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.Name, "name", "", "Deployment name (default: provider)")
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
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" { name = "local" }
			lokad, err := findBinary("lokad")
			if err != nil { return err }
			store, _ := loadDeployments()
			store.Add(Deployment{Name: name, Provider: "local", Endpoint: "http://localhost:8080", Workers: 1, Status: "running", CreatedAt: time.Now()})
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
			fmt.Printf("  Endpoint: http://localhost:8080\n")
			fmt.Printf("  Stop:     loka deploy down\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "local", "Deployment name")
	cmd.Flags().BoolVarP(&foreground, "foreground", "f", false, "Foreground")
	return cmd
}

func newDeployListCmd() *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List deployments", Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			store, _ := loadDeployments()
			if len(store.Deployments) == 0 {
				fmt.Println("No deployments. Create one: loka deploy local --name dev")
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

func newDeployUseCmd() *cobra.Command {
	return &cobra.Command{
		Use: "use <name>", Short: "Switch active deployment", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, _ := loadDeployments()
			if err := store.SetActive(args[0]); err != nil { return err }
			saveDeployments(store)
			d := store.GetActive()
			fmt.Printf("Active deployment: %q (%s, %s)\n", d.Name, d.Provider, d.Endpoint)
			return nil
		},
	}
}

func newDeployRenameCmd() *cobra.Command {
	return &cobra.Command{
		Use: "rename <old> <new>", Short: "Rename a deployment", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, _ := loadDeployments()
			d := store.Get(args[0])
			if d == nil { return fmt.Errorf("deployment %q not found", args[0]) }
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
		Use: "down [name]", Short: "Stop a deployment", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, _ := loadDeployments()
			name := store.Active; if len(args) > 0 { name = args[0] }
			d := store.Get(name)
			if d == nil { return fmt.Errorf("deployment %q not found", name) }
			if d.Provider == "local" {
				out, _ := exec.Command("pgrep", "-f", "lokad").Output()
				if len(out) > 0 { exec.Command("kill", strings.TrimSpace(string(out))).Run(); fmt.Printf("LOKA %q stopped\n", name) } else { fmt.Printf("%q is not running\n", name) }
			}
			d.Status = "stopped"; saveDeployments(store)
			return nil
		},
	}
}

func newDeployStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use: "status [name]", Short: "Show deployment status", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, _ := loadDeployments()
			name := store.Active; if len(args) > 0 { name = args[0] }
			d := store.Get(name)
			if d == nil { return fmt.Errorf("deployment %q not found", name) }
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
		Use: "destroy [name]", Short: "Destroy a deployment", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, _ := loadDeployments()
			name := store.Active; if len(args) > 0 { name = args[0] }
			d := store.Get(name)
			if d == nil { return fmt.Errorf("deployment %q not found", name) }
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
			fmt.Printf("Deployment %q destroyed\n", name)
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
