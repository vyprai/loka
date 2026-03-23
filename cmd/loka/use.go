package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use <deployment>",
		Short: "Switch active deployment",
		Long:  "Switch which deployment all commands target. Same as 'loka deploy use'.",
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
