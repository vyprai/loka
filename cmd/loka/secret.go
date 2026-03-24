package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/vyprai/loka/internal/secret"
)

func newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage credentials and secrets",
		Long: `Manage credentials stored in ~/.loka/credentials.yaml.

Examples:
  loka secret set db-url --type env --value "postgres://..."
  loka secret set aws-prod --type aws --access-key AKIA... --secret-key ...
  loka secret list
  loka secret remove db-url`,
	}

	cmd.AddCommand(
		newSecretSetCmd(),
		newSecretListCmd(),
		newSecretRemoveCmd(),
	)
	return cmd
}

func newSecretSetCmd() *cobra.Command {
	var (
		secretType string
		value      string
		accessKey  string
		secretKey  string
		region     string
	)

	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Set a secret",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			store := secret.NewStore()

			sec := secret.Secret{
				Name:      name,
				Type:      secretType,
				Value:     value,
				AccessKey: accessKey,
				SecretKey: secretKey,
				Region:    region,
			}

			if secretType == "aws" && accessKey == "" {
				return fmt.Errorf("--access-key is required for aws secrets")
			}
			if secretType == "aws" && secretKey == "" {
				return fmt.Errorf("--secret-key is required for aws secrets")
			}
			if secretType == "env" && value == "" {
				return fmt.Errorf("--value is required for env secrets")
			}

			if err := store.Set(sec); err != nil {
				return err
			}
			fmt.Printf("Secret %q saved\n", name)
			return nil
		},
	}

	cmd.Flags().StringVar(&secretType, "type", "env", "Secret type: env, aws, gcs")
	cmd.Flags().StringVar(&value, "value", "", "Secret value (for env type)")
	cmd.Flags().StringVar(&accessKey, "access-key", "", "AWS access key ID")
	cmd.Flags().StringVar(&secretKey, "secret-key", "", "AWS secret access key")
	cmd.Flags().StringVar(&region, "region", "", "AWS region")
	return cmd
}

func newSecretListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List secrets",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			store := secret.NewStore()
			secrets, err := store.List()
			if err != nil {
				return err
			}
			if len(secrets) == 0 {
				fmt.Println("No secrets stored. Add one: loka secret set <name> --type env --value ...")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tTYPE\tDETAILS")
			for _, s := range secrets {
				details := ""
				switch s.Type {
				case "aws":
					details = fmt.Sprintf("access_key=%s", s.AccessKey)
					if s.Region != "" {
						details += fmt.Sprintf(" region=%s", s.Region)
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", s.Name, s.Type, details)
			}
			w.Flush()
			return nil
		},
	}
}

func newSecretRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "remove <name>",
		Short:   "Remove a secret",
		Aliases: []string{"rm", "delete"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store := secret.NewStore()
			if err := store.Remove(args[0]); err != nil {
				return err
			}
			fmt.Printf("Secret %q removed\n", args[0])
			return nil
		},
	}
}
