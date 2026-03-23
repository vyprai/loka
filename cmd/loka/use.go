package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

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
