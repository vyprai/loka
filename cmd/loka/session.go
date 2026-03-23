package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/vyprai/loka/pkg/lokaapi"
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
		mounts          []string
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
			for _, m := range mounts {
				mount, err := parseMount(m)
				if err != nil {
					return err
				}
				req.Mounts = append(req.Mounts, mount)
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
	cmd.Flags().StringArrayVar(&mounts, "mount", nil, `Mount object storage (repeatable). Format: provider://bucket/prefix@/mount/path[:ro]
  Credentials and options are passed as query params.
  Examples:
    --mount s3://my-bucket@/data?access_key_id=AKIA...&secret_access_key=...
    --mount s3://my-bucket/datasets@/data:ro?region=us-east-1
    --mount gcs://my-bucket@/gcs-data?service_account_json=@/path/to/key.json
    --mount azure-blob://container@/az?account_name=...&account_key=...
    --mount s3://my-bucket@/data?endpoint=http://minio:9000&access_key_id=minioadmin&secret_access_key=minioadmin`)
	return cmd
}

// parseMount parses a mount string like "s3://bucket/prefix@/mount/path:ro?endpoint=..."
func parseMount(s string) (lokaapi.StorageMount, error) {
	mount := lokaapi.StorageMount{}

	// Split off query params.
	query := ""
	if idx := strings.Index(s, "?"); idx != -1 {
		query = s[idx+1:]
		s = s[:idx]
	}

	// Split provider://bucket/prefix@/mount/path[:ro]
	parts := strings.SplitN(s, "://", 2)
	if len(parts) != 2 {
		return mount, fmt.Errorf("invalid mount format: %s (expected provider://bucket@/path)", s)
	}
	mount.Provider = parts[0]

	rest := parts[1]
	atParts := strings.SplitN(rest, "@", 2)
	if len(atParts) != 2 {
		return mount, fmt.Errorf("invalid mount format: %s (missing @/mount/path)", s)
	}

	bucketPrefix := atParts[0]
	mountPath := atParts[1]

	// Split bucket and prefix.
	if idx := strings.Index(bucketPrefix, "/"); idx != -1 {
		mount.Bucket = bucketPrefix[:idx]
		mount.Prefix = bucketPrefix[idx+1:]
	} else {
		mount.Bucket = bucketPrefix
	}

	// Check for :ro suffix.
	if strings.HasSuffix(mountPath, ":ro") {
		mount.ReadOnly = true
		mountPath = strings.TrimSuffix(mountPath, ":ro")
	}
	mount.MountPath = mountPath

	// Parse query params — known keys go to typed fields, rest go to credentials.
	creds := map[string]string{}
	for _, kv := range strings.Split(query, "&") {
		if kv == "" {
			continue
		}
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "endpoint":
			mount.Endpoint = v
		case "region":
			mount.Region = v
		case "access_key_id", "secret_access_key", "session_token",
			"service_account_json",
			"account_name", "account_key", "sas_token":
			// If value starts with @, read from file.
			if strings.HasPrefix(v, "@") {
				data, err := os.ReadFile(strings.TrimPrefix(v, "@"))
				if err != nil {
					return mount, fmt.Errorf("read credential file %s: %w", v, err)
				}
				v = strings.TrimSpace(string(data))
			}
			creds[k] = v
		default:
			creds[k] = v
		}
	}
	if len(creds) > 0 {
		mount.Credentials = creds
	}

	return mount, nil
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
