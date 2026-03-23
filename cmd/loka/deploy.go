package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newDeployCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy LOKA to cloud infrastructure",
		Long: `Deploy a LOKA control plane and workers to cloud providers.

Examples:
  loka deploy aws --region us-east-1 --workers 3
  loka deploy gcp --project my-project --zone us-central1-a
  loka deploy local
  loka deploy status
  loka deploy destroy`,
	}
	cmd.AddCommand(
		newDeployCloudCmd("aws", "Deploy to AWS (EC2 instances)", deployAWS),
		newDeployCloudCmd("gcp", "Deploy to Google Cloud (Compute Engine)", deployGCP),
		newDeployCloudCmd("azure", "Deploy to Azure (Virtual Machines)", deployAzure),
		newDeployCloudCmd("digitalocean", "Deploy to DigitalOcean (Droplets)", deployDigitalOcean),
		newDeployCloudCmd("ovh", "Deploy to OVH (Bare Metal)", deployOVH),
		newDeployLocalCmd(),
		newDeployStatusCmd(),
		newDeployDestroyCmd(),
	)
	return cmd
}

// ── Cloud deploy template ───────────────────────────────

type deployFunc func(opts deployOpts) error

type deployOpts struct {
	Provider     string
	Region       string
	Zone         string
	Project      string
	Workers      int
	InstanceType string
	SSHKey       string
}

func newDeployCloudCmd(provider, description string, fn deployFunc) *cobra.Command {
	var opts deployOpts
	opts.Provider = provider

	cmd := &cobra.Command{
		Use:   provider,
		Short: description,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fn(opts)
		},
	}

	cmd.Flags().StringVar(&opts.Region, "region", "", "Cloud region")
	cmd.Flags().StringVar(&opts.Zone, "zone", "", "Cloud zone")
	cmd.Flags().StringVar(&opts.Project, "project", "", "Cloud project ID (GCP)")
	cmd.Flags().IntVar(&opts.Workers, "workers", 1, "Number of worker nodes")
	cmd.Flags().StringVar(&opts.InstanceType, "instance-type", "", "Instance type (default: provider-specific)")
	cmd.Flags().StringVar(&opts.SSHKey, "ssh-key", "", "SSH key name for instance access")

	return cmd
}

// ── Local deploy ────────────────────────────────────────

func newDeployLocalCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "local",
		Short: "Deploy LOKA locally (single node)",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("Deploying LOKA locally...")
			fmt.Println("")
			fmt.Println("  1. Starting lokad with embedded worker")
			fmt.Println("  2. Database: SQLite")
			fmt.Println("  3. Listening on :8080")
			fmt.Println("")
			fmt.Println("Run:")
			fmt.Println("  lokad")
			fmt.Println("")
			fmt.Println("Or with the install script:")
			fmt.Println("  curl -fsSL https://rizqme.github.io/loka/install.sh | bash")
			fmt.Println("  lokad")
			return nil
		},
	}
}

// ── Status ──────────────────────────────────────────────

func newDeployStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show deployment status",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			var health struct {
				Status       string `json:"status"`
				WorkersTotal int    `json:"workers_total"`
				WorkersReady int    `json:"workers_ready"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/health", nil, &health); err != nil {
				return fmt.Errorf("cannot reach control plane at %s: %w", serverAddr, err)
			}

			fmt.Printf("Control Plane: %s\n", serverAddr)
			fmt.Printf("Status:        %s\n", health.Status)
			fmt.Printf("Workers:       %d ready / %d total\n", health.WorkersReady, health.WorkersTotal)

			// List workers
			var resp struct {
				Workers []struct {
					ID       string `json:"ID"`
					Hostname string `json:"Hostname"`
					Provider string `json:"Provider"`
					Region   string `json:"Region"`
					Status   string `json:"Status"`
				} `json:"workers"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/workers", nil, &resp); err == nil && len(resp.Workers) > 0 {
				fmt.Println("")
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "WORKER\tHOSTNAME\tPROVIDER\tREGION\tSTATUS")
				for _, wk := range resp.Workers {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
						shortID(wk.ID), wk.Hostname, wk.Provider, wk.Region, wk.Status)
				}
				w.Flush()
			}
			return nil
		},
	}
}

// ── Destroy ─────────────────────────────────────────────

func newDeployDestroyCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "destroy",
		Short: "Tear down a LOKA deployment",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !force {
				fmt.Print("This will destroy all LOKA infrastructure. Type 'yes' to confirm: ")
				var confirm string
				fmt.Scanln(&confirm)
				if strings.ToLower(confirm) != "yes" {
					fmt.Println("Aborted.")
					return nil
				}
			}
			fmt.Println("Destroying LOKA deployment...")
			fmt.Println("  (Cloud resource teardown not yet implemented)")
			fmt.Println("  For now, manually terminate instances and remove lokad.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Skip confirmation")
	return cmd
}

// ── Cloud deploy implementations ────────────────────────

func deployAWS(opts deployOpts) error {
	region := opts.Region
	if region == "" {
		region = "us-east-1"
	}
	instanceType := opts.InstanceType
	if instanceType == "" {
		instanceType = "i3.metal"
	}

	fmt.Printf("Deploying LOKA to AWS (%s)\n", region)
	fmt.Println("")
	fmt.Printf("  Control plane: 1x t3.medium (%s)\n", region)
	fmt.Printf("  Workers:       %dx %s (%s)\n", opts.Workers, instanceType, region)
	fmt.Println("")
	fmt.Println("Steps:")
	fmt.Println("  1. Create VPC + security groups")
	fmt.Println("  2. Launch control plane instance")
	fmt.Println("  3. Install LOKA via install script")
	fmt.Println("  4. Launch worker instances with KVM")
	fmt.Println("  5. Register workers with control plane")
	fmt.Println("")
	fmt.Println("  (Automated provisioning coming soon.)")
	fmt.Println("")
	fmt.Println("Manual setup:")
	fmt.Printf("  # On control plane instance:\n")
	fmt.Printf("  curl -fsSL https://rizqme.github.io/loka/install.sh | bash\n")
	fmt.Printf("  lokad\n")
	fmt.Println("")
	fmt.Printf("  # On each worker:\n")
	fmt.Printf("  curl -fsSL https://rizqme.github.io/loka/install.sh | bash\n")
	fmt.Printf("  loka-worker --control-plane <cp-ip>:9090\n")
	return nil
}

func deployGCP(opts deployOpts) error {
	zone := opts.Zone
	if zone == "" {
		zone = "us-central1-a"
	}
	instanceType := opts.InstanceType
	if instanceType == "" {
		instanceType = "n2-standard-8"
	}

	fmt.Printf("Deploying LOKA to GCP (%s)\n", zone)
	fmt.Println("")
	fmt.Printf("  Control plane: 1x e2-standard-2 (%s)\n", zone)
	fmt.Printf("  Workers:       %dx %s with nested virtualization (%s)\n", opts.Workers, instanceType, zone)
	fmt.Println("")
	fmt.Println("  (Automated provisioning coming soon.)")
	return nil
}

func deployAzure(opts deployOpts) error {
	region := opts.Region
	if region == "" {
		region = "eastus"
	}
	fmt.Printf("Deploying LOKA to Azure (%s)\n", region)
	fmt.Printf("  Workers: %dx Standard_D8s_v3 with nested virtualization\n", opts.Workers)
	fmt.Println("  (Automated provisioning coming soon.)")
	return nil
}

func deployDigitalOcean(opts deployOpts) error {
	region := opts.Region
	if region == "" {
		region = "nyc1"
	}
	fmt.Printf("Deploying LOKA to DigitalOcean (%s)\n", region)
	fmt.Printf("  Workers: %dx s-8vcpu-16gb\n", opts.Workers)
	fmt.Println("  (Automated provisioning coming soon.)")
	return nil
}

func deployOVH(opts deployOpts) error {
	fmt.Println("Deploying LOKA to OVH")
	fmt.Printf("  Workers: %dx bare metal\n", opts.Workers)
	fmt.Println("  (Automated provisioning coming soon.)")
	return nil
}
