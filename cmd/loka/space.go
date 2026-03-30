package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newSpaceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "space",
		Short: "Manage LOKA spaces (servers/deployments)",
	}
	cmd.AddCommand(
		newSpaceListCmd(),
		newSpaceUseCmd(),
		newSpaceCurrentCmd(),
		newDeployDownCmd(),
		newDeployStatusCmd(),
		newDeployDestroyCmd(),
		newSpaceCreateCmd(),
		newConnectCmd(),
		newProviderCmd(),
		newDeployFileCmd(),
		newDeployExportCmd(),
		newDeployRenameCmd(),
	)
	return cmd
}

func newSpaceCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new LOKA space",
		Long: `Create a new LOKA space on local, cloud, or VM infrastructure.

  loka space create local --name dev
  loka space create aws --name prod --region us-east-1 --workers 3
  loka space create vm --name staging --cp 10.0.0.1`,
	}
	cmd.AddCommand(
		newDeployLocalCmd(),
		newDeployCloudCmd("aws", "Deploy to AWS (EC2)", deployAWS),
		newDeployCloudCmd("gcp", "Deploy to Google Cloud", deployGCP),
		newDeployCloudCmd("azure", "Deploy to Azure", deployAzure),
		newDeployCloudCmd("do", "Deploy to DigitalOcean", deployDigitalOcean),
		newDeployCloudCmd("ovh", "Deploy to OVH", deployOVH),
		newDeployVMCmd(),
	)
	return cmd
}

func newSpaceListCmd() *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List spaces", Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			store, _ := loadDeployments()
			if len(store.Deployments) == 0 {
				fmt.Println("No spaces. Set one up: loka space create local --name dev")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "  NAME\tSPACE\tENDPOINT\tWORKERS\tSTATUS\tCREATED")
			for _, d := range store.Deployments {
				a := " "
				if d.Name == store.Active {
					a = "*"
				}
				fmt.Fprintf(w, "%s %s\t%s\t%s\t%d\t%s\t%s\n", a, d.Name, d.Provider, d.Endpoint, d.Workers, d.Status, d.CreatedAt.Format("2006-01-02"))
			}
			w.Flush()
			return nil
		},
	}
}

func newSpaceCurrentCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "current",
		Short: "Show the active space",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, _ := loadDeployments()
			d := store.GetActive()
			if d == nil {
				fmt.Println("No active space. Set one up: loka space create local")
				return nil
			}
			fmt.Printf("Name:     %s\n", d.Name)
			fmt.Printf("Space:    %s\n", d.Provider)
			fmt.Printf("Endpoint: %s\n", d.Endpoint)
			fmt.Printf("Workers:  %d\n", d.Workers)
			fmt.Printf("Status:   %s\n", d.Status)
			return nil
		},
	}
}

func newSpaceUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "select <space>",
		Short:   "Switch active space",
		Long:    "Switch which space all commands target.",
		Aliases: []string{"use"},
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := loadDeployments()
			if err != nil {
				return err
			}
			if err := store.SetActive(args[0]); err != nil {
				return err
			}
			saveDeployments(store)
			d := store.GetActive()
			fmt.Printf("Active: %s (%s, %s)\n", d.Name, d.Provider, d.Endpoint)
			return nil
		},
	}
}
