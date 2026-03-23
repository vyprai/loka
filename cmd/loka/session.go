package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/rizqme/loka/pkg/lokaapi"
)

func newSessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Manage LOKA sessions",
	}
	cmd.AddCommand(
		newSessionCreateCmd(),
		newSessionListCmd(),
		newSessionGetCmd(),
		newSessionDestroyCmd(),
		newSessionPauseCmd(),
		newSessionResumeCmd(),
		newSessionModeCmd(),
	)
	return cmd
}

func newSessionCreateCmd() *cobra.Command {
	var (
		name       string
		image      string
		snapshotID string
		mode       string
		vcpus           int
		memoryMB        int
		allowedCommands string
		blockedCommands string
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new session",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			req := lokaapi.CreateSessionReq{
				Name:       name,
				Image:      image,
				SnapshotID: snapshotID,
				Mode:       mode,
				VCPUs:    vcpus,
				MemoryMB: memoryMB,
			}
			if allowedCommands != "" {
				req.AllowedCommands = strings.Split(allowedCommands, ",")
			}
			if blockedCommands != "" {
				req.BlockedCommands = strings.Split(blockedCommands, ",")
			}
			sess, err := client.CreateSession(cmd.Context(), req)
			if err != nil {
				return err
			}
			if outputFmt == "json" {
				return printJSON(sess)
			}
			fmt.Printf("Session created: %s\n", sess.ID)
			fmt.Printf("  Name:     %s\n", sess.Name)
			fmt.Printf("  Status:   %s\n", sess.Status)
			fmt.Printf("  Mode:     %s\n", sess.Mode)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Session name")
	cmd.Flags().StringVar(&image, "image", "", "Docker image (e.g., ubuntu:22.04, python:3.12-slim)")
	cmd.Flags().StringVar(&snapshotID, "snapshot", "", "Restore from a snapshot ID")
	cmd.Flags().StringVar(&mode, "mode", "explore", "Execution mode (explore, execute, ask)")
	cmd.Flags().IntVar(&vcpus, "vcpus", 1, "Number of vCPUs")
	cmd.Flags().IntVar(&memoryMB, "memory", 512, "Memory in MB")
	cmd.Flags().StringVar(&allowedCommands, "allowed-commands", "", "Comma-separated allowlist of commands")
	cmd.Flags().StringVar(&blockedCommands, "blocked-commands", "", "Comma-separated blocklist of commands")
	return cmd
}

func newSessionListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List sessions",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			resp, err := client.ListSessions(cmd.Context())
			if err != nil {
				return err
			}
			if outputFmt == "json" {
				return printJSON(resp)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tSTATUS\tMODE\tCREATED")
			for _, s := range resp.Sessions {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					shortID(s.ID), s.Name, s.Status, s.Mode, s.CreatedAt.Format("2006-01-02 15:04:05"))
			}
			w.Flush()
			return nil
		},
	}
}

func newSessionGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <session-id>",
		Short: "Get session details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			sess, err := client.GetSession(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if outputFmt == "json" {
				return printJSON(sess)
			}
			fmt.Printf("ID:        %s\n", sess.ID)
			fmt.Printf("Name:      %s\n", sess.Name)
			fmt.Printf("Status:    %s\n", sess.Status)
			fmt.Printf("Mode:      %s\n", sess.Mode)
			fmt.Printf("Worker:    %s\n", sess.WorkerID)
			fmt.Printf("Image:     %s\n", sess.ImageRef)
			fmt.Printf("vCPUs:     %d\n", sess.VCPUs)
			fmt.Printf("Memory:    %d MB\n", sess.MemoryMB)
			fmt.Printf("Created:   %s\n", sess.CreatedAt.Format("2006-01-02 15:04:05"))
			return nil
		},
	}
}

func newSessionDestroyCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "destroy <session-id>",
		Short:   "Destroy a session",
		Aliases: []string{"rm", "delete"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			if err := client.DestroySession(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Printf("Session %s destroyed\n", shortID(args[0]))
			return nil
		},
	}
}

func newSessionPauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pause <session-id>",
		Short: "Pause a running session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			sess, err := client.PauseSession(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Printf("Session %s paused (status: %s)\n", shortID(sess.ID), sess.Status)
			return nil
		},
	}
}

func newSessionResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume <session-id>",
		Short: "Resume a paused session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			sess, err := client.ResumeSession(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Printf("Session %s resumed (status: %s)\n", shortID(sess.ID), sess.Status)
			return nil
		},
	}
}

func newSessionModeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mode <session-id> [mode]",
		Short: "Get or set session execution mode",
		Long:  "Modes: inspect, plan, execute, commit",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			if len(args) == 1 {
				// Get current mode.
				sess, err := client.GetSession(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				fmt.Printf("Mode: %s\n", sess.Mode)
				return nil
			}
			sess, err := client.SetSessionMode(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			fmt.Printf("Session %s mode set to: %s\n", shortID(sess.ID), sess.Mode)
			return nil
		},
	}
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
