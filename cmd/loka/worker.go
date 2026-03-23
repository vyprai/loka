package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newWorkerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Manage workers",
	}
	cmd.AddCommand(
		newWorkerAddCmd(),
		newWorkerRemoveByAddrCmd(),
		newWorkerScaleCmd(),
		newWorkerListCmd(),
		newWorkerGetCmd(),
		newWorkerDrainCmd(),
		newWorkerUndrainCmd(),
		newWorkerDeregisterCmd(),
		newWorkerLabelCmd(),
		newWorkerTopCmd(),
	)
	return cmd
}

func newWorkerListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List workers",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			resp, err := client.ListWorkers(cmd.Context())
			if err != nil {
				return err
			}
			if outputFmt == "json" {
				return printJSON(resp)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tHOSTNAME\tPROVIDER\tREGION\tSTATUS\tCPU\tMEMORY\tLABELS")
			for _, wk := range resp.Workers {
				labels := formatLabels(wk.Labels)
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%dMB\t%s\n",
					shortID(wk.ID), wk.Hostname, wk.Provider, wk.Region,
					wk.Status, wk.Capacity.CPUCores, wk.Capacity.MemoryMB, labels)
			}
			w.Flush()
			return nil
		},
	}
}

func newWorkerGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <worker-id>",
		Short: "Get worker details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			w, err := client.GetWorker(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if outputFmt == "json" {
				return printJSON(w)
			}
			fmt.Printf("ID:        %s\n", w.ID)
			fmt.Printf("Hostname:  %s\n", w.Hostname)
			fmt.Printf("Provider:  %s\n", w.Provider)
			fmt.Printf("Region:    %s\n", w.Region)
			fmt.Printf("Status:    %s\n", w.Status)
			fmt.Printf("CPU:       %d cores\n", w.Capacity.CPUCores)
			fmt.Printf("Memory:    %d MB\n", w.Capacity.MemoryMB)
			fmt.Printf("Disk:      %d MB\n", w.Capacity.DiskMB)
			fmt.Printf("KVM:       %v\n", w.KVMAvailable)
			fmt.Printf("Labels:    %s\n", formatLabels(w.Labels))
			fmt.Printf("Last Seen: %s\n", w.LastSeen.Format("2006-01-02 15:04:05"))
			return nil
		},
	}
}

func newWorkerDrainCmd() *cobra.Command {
	var timeout int

	cmd := &cobra.Command{
		Use:   "drain <worker-id>",
		Short: "Drain a worker (migrate sessions to other workers)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			w, err := client.DrainWorker(cmd.Context(), args[0], timeout)
			if err != nil {
				return err
			}
			fmt.Printf("Worker %s drain started (status: %s)\n", shortID(w.ID), w.Status)
			return nil
		},
	}
	cmd.Flags().IntVar(&timeout, "timeout", 300, "Drain timeout in seconds")
	return cmd
}

func newWorkerUndrainCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "undrain <worker-id>",
		Short: "Re-enable a draining worker",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			w, err := client.UndrainWorker(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Printf("Worker %s undrained (status: %s)\n", shortID(w.ID), w.Status)
			return nil
		},
	}
}

func newWorkerDeregisterCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "deregister <worker-id>",
		Short: "Deregister a worker from the control plane by ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			if err := client.RemoveWorker(cmd.Context(), args[0], force); err != nil {
				return err
			}
			fmt.Printf("Worker %s removed\n", shortID(args[0]))
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Force remove (kill active sessions)")
	return cmd
}

func newWorkerLabelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "label <worker-id> key=value [key=value...]",
		Short: "Add or remove labels (empty value removes label)",
		Long:  "Examples:\n  loka worker label <id> gpu=true tier=premium\n  loka worker label <id> gpu=  (removes gpu label)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			labels := make(map[string]string)
			for _, arg := range args[1:] {
				parts := strings.SplitN(arg, "=", 2)
				if len(parts) != 2 {
					return fmt.Errorf("invalid label format: %s (use key=value)", arg)
				}
				labels[parts[0]] = parts[1]
			}
			w, err := client.LabelWorker(cmd.Context(), args[0], labels)
			if err != nil {
				return err
			}
			fmt.Printf("Worker %s labels updated: %s\n", shortID(w.ID), formatLabels(w.Labels))
			return nil
		},
	}
}

func newWorkerTopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "top",
		Short: "Show resource usage across all workers",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			resp, err := client.ListWorkers(cmd.Context())
			if err != nil {
				return err
			}
			if len(resp.Workers) == 0 {
				fmt.Println("No workers")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "WORKER\tPROVIDER\tREGION\tCPU\tMEMORY\tSTATUS\tLAST SEEN")
			for _, wk := range resp.Workers {
				fmt.Fprintf(w, "%s\t%s\t%s\t%d cores\t%d MB\t%s\t%s\n",
					shortID(wk.ID), wk.Provider, wk.Region,
					wk.Capacity.CPUCores, wk.Capacity.MemoryMB,
					wk.Status, wk.LastSeen.Format("15:04:05"))
			}
			w.Flush()
			return nil
		},
	}
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "-"
	}
	var parts []string
	for k, v := range labels {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ",")
}
