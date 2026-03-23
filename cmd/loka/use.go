package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newCurrentCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "current",
		Short: "Show the active server",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, _ := loadDeployments()
			d := store.GetActive()
			if d == nil {
				fmt.Println("No active server. Deploy one: loka deploy local")
				return nil
			}
			fmt.Printf("Name:     %s\n", d.Name)
			fmt.Printf("Provider: %s\n", d.Provider)
			fmt.Printf("Endpoint: %s\n", d.Endpoint)
			fmt.Printf("Workers:  %d\n", d.Workers)
			fmt.Printf("Status:   %s\n", d.Status)
			return nil
		},
	}
}

func newUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use <server>",
		Short: "Switch active server",
		Long:  "Switch which server all commands target.",
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
