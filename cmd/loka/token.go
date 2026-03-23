package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newTokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage worker registration tokens",
	}
	cmd.AddCommand(
		newTokenCreateCmd(),
		newTokenListCmd(),
		newTokenRevokeCmd(),
	)
	return cmd
}

func newTokenCreateCmd() *cobra.Command {
	var (
		name    string
		expires int
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a worker registration token",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			var resp struct {
				ID    string `json:"ID"`
				Name  string `json:"Name"`
				Token string `json:"Token"`
			}
			err := client.Raw(cmd.Context(), "POST", "/api/v1/worker-tokens", map[string]any{
				"name":            name,
				"expires_seconds": expires,
			}, &resp)
			if err != nil {
				return err
			}
			if outputFmt == "json" {
				return printJSON(resp)
			}
			fmt.Printf("Token created: %s\n", resp.ID)
			fmt.Printf("  Name:  %s\n", resp.Name)
			fmt.Printf("  Token: %s\n", resp.Token)
			fmt.Println("")
			fmt.Println("Use this token to register a self-managed worker:")
			fmt.Printf("  loka-worker --token %s --control-plane <address>\n", resp.Token)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Token name")
	cmd.Flags().IntVar(&expires, "expires", 86400, "Expiry in seconds (default 24h)")
	return cmd
}

func newTokenListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List worker registration tokens",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			var resp struct {
				Tokens []struct {
					ID        string `json:"id"`
					Name      string `json:"name"`
					Token     string `json:"token"`
					Used      bool   `json:"used"`
					WorkerID  string `json:"worker_id"`
					CreatedAt string `json:"created_at"`
				} `json:"tokens"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/worker-tokens", nil, &resp); err != nil {
				return err
			}
			if outputFmt == "json" {
				return printJSON(resp)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tTOKEN\tUSED\tWORKER")
			for _, t := range resp.Tokens {
				used := "no"
				if t.Used {
					used = "yes"
				}
				worker := "-"
				if t.WorkerID != "" {
					worker = shortID(t.WorkerID)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					shortID(t.ID), t.Name, t.Token, used, worker)
			}
			w.Flush()
			return nil
		},
	}
}

func newTokenRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <token-id>",
		Short: "Revoke a worker registration token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			if err := client.Raw(cmd.Context(), "DELETE", "/api/v1/worker-tokens/"+args[0], nil, nil); err != nil {
				return err
			}
			fmt.Printf("Token %s revoked\n", shortID(args[0]))
			return nil
		},
	}
}
