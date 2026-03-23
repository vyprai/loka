package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

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
		newSessionIdleCmd(),
		newSessionMountLocalCmd(),
		newSessionPortForwardCmd(),
		newSessionPortsCmd(),
		newSessionExposeCmd(),
		newSessionUnexposeCmd(),
		newSessionSyncCmd(),
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
		ports           []string
		idleTimeout     int
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
			for _, p := range ports {
				pm, err := parsePortMap(p)
				if err != nil {
					return err
				}
				req.Ports = append(req.Ports, lokaapi.PortMapping{
					LocalPort:  pm.local,
					RemotePort: pm.remote,
				})
			}
			sess, err := client.CreateSession(cmd.Context(), req)
			if err != nil {
				return err
			}

			// Wait for session to be ready (polls until ready or error).
			if !sess.Ready {
				if sess.StatusMessage != "" {
					fmt.Printf("  %s...", sess.StatusMessage)
				} else {
					fmt.Print("  Starting...")
				}
				for !sess.Ready && sess.Status != "error" {
					time.Sleep(500 * time.Millisecond)
					updated, err := client.GetSession(cmd.Context(), sess.ID)
					if err != nil {
						fmt.Println()
						return err
					}
					if updated.StatusMessage != "" && updated.StatusMessage != sess.StatusMessage {
						fmt.Printf("\n  %s...", updated.StatusMessage)
					} else {
						fmt.Print(".")
					}
					sess = updated
				}
				fmt.Println(" ready!")
			}

			if outputFmt == "json" {
				return printJSON(sess)
			}
			fmt.Printf("Session created: %s\n", sess.ID)
			fmt.Printf("  Status:   %s\n", sess.Status)
			fmt.Printf("  Mode:     %s\n", sess.Mode)
			if sess.Name != "" {
				fmt.Printf("  Name:     %s\n", sess.Name)
			}
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
	cmd.Flags().StringArrayVar(&ports, "port", nil, "Port forwarding (repeatable, format: local:remote, e.g. --port 8080:5000)")
	cmd.Flags().IntVar(&idleTimeout, "idle-timeout", 0, "Auto-idle after N seconds of inactivity (0 = never)")
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

func newSessionIdleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "idle <session-id>",
		Short: "Suspend a session (auto-wakes on access)",
		Long: `Put a session into idle state. The VM is suspended to save resources.
The session automatically wakes when accessed (exec, port-forward, sync, domain proxy).

Examples:
  loka session idle <id>`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			var sess struct {
				ID     string `json:"ID"`
				Status string `json:"Status"`
			}
			if err := client.Raw(cmd.Context(), "POST", "/api/v1/sessions/"+args[0]+"/idle", nil, &sess); err != nil {
				return err
			}
			fmt.Printf("Session %s is now idle (auto-wakes on access)\n", shortID(sess.ID))
			return nil
		},
	}
}

func newSessionExposeCmd() *cobra.Command {
	var port int

	cmd := &cobra.Command{
		Use:   "expose <session-id> <subdomain>",
		Short: "Expose a session port via subdomain",
		Long: `Map a subdomain to a port inside a session, making it accessible via HTTP.

The control plane acts as a reverse proxy:
  <subdomain>.<base-domain> → session VM port

Examples:
  loka session expose <id> my-app --port 5000
  loka session expose <id> api --port 8080

Access at: https://my-app.loka.example.com`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := args[0]
			subdomain := args[1]

			if port <= 0 {
				return fmt.Errorf("--port is required")
			}

			client := newClient()
			var resp struct {
				Subdomain string `json:"subdomain"`
				URL       string `json:"url"`
				Port      int    `json:"port"`
			}
			err := client.Raw(cmd.Context(), "POST", "/api/v1/sessions/"+sessionID+"/expose", map[string]any{
				"subdomain":   subdomain,
				"remote_port": port,
			}, &resp)
			if err != nil {
				return err
			}

			fmt.Printf("Exposed: %s → session %s port %d\n", resp.URL, shortID(sessionID), resp.Port)
			return nil
		},
	}

	cmd.Flags().IntVarP(&port, "port", "p", 0, "Port inside the VM to expose (required)")
	return cmd
}

func newSessionUnexposeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unexpose <session-id> <subdomain>",
		Short: "Remove a subdomain exposure",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := args[0]
			subdomain := args[1]

			client := newClient()
			if err := client.Raw(cmd.Context(), "DELETE", "/api/v1/sessions/"+sessionID+"/expose/"+subdomain, nil, nil); err != nil {
				return err
			}
			fmt.Printf("Removed: %s\n", subdomain)
			return nil
		},
	}
}

func newSessionPortsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ports <session-id>",
		Short: "List port mappings for a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			sess, err := client.GetSession(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if outputFmt == "json" {
				return printJSON(sess.Ports)
			}
			if len(sess.Ports) == 0 {
				fmt.Println("No port mappings.")
				fmt.Println("Add ports: loka session port-forward <id> 8080:5000")
				return nil
			}
			for _, p := range sess.Ports {
				proto := p.Protocol
				if proto == "" {
					proto = "tcp"
				}
				fmt.Printf("  %d → %d (%s)\n", p.LocalPort, p.RemotePort, proto)
			}
			return nil
		},
	}
}

func newDomainsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "domains",
		Short: "List all domain routes",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			var resp struct {
				Routes []struct {
					Subdomain  string `json:"subdomain"`
					SessionID  string `json:"session_id"`
					RemotePort int    `json:"remote_port"`
				} `json:"routes"`
				BaseDomain string `json:"base_domain"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/domains", nil, &resp); err != nil {
				return err
			}
			if outputFmt == "json" {
				return printJSON(resp)
			}
			if len(resp.Routes) == 0 {
				fmt.Println("No domain routes.")
				fmt.Println("Expose a session: loka session expose <id> my-app --port 5000")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SUBDOMAIN\tSESSION\tPORT\tURL")
			for _, r := range resp.Routes {
				fmt.Fprintf(w, "%s\t%s\t%d\t%s.%s\n",
					r.Subdomain, shortID(r.SessionID), r.RemotePort, r.Subdomain, resp.BaseDomain)
			}
			w.Flush()
			return nil
		},
	}
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func newSessionSyncCmd() *cobra.Command {
	var (
		direction string
		prefix    string
		delete    bool
		dryRun    bool
	)

	cmd := &cobra.Command{
		Use:   "sync <session-id> <mount-path>",
		Short: "Sync data between a session mount and object storage",
		Long: `Push changed files from the VM back to the bucket, or pull latest from the bucket.

Examples:
  loka session sync <id> /data --direction push        # VM → bucket
  loka session sync <id> /data --direction pull        # bucket → VM
  loka session sync <id> /data --direction push --prefix results/
  loka session sync <id> /data --direction push --delete   # mirror (delete extra files)
  loka session sync <id> /data --direction push --dry-run  # preview changes`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := args[0]
			mountPath := args[1]

			if direction != "push" && direction != "pull" {
				return fmt.Errorf("--direction must be 'push' or 'pull'")
			}

			client := newClient()
			var result struct {
				MountPath        string   `json:"mount_path"`
				Direction        string   `json:"direction"`
				FilesAdded       int      `json:"files_added"`
				FilesUpdated     int      `json:"files_updated"`
				FilesDeleted     int      `json:"files_deleted"`
				BytesTransferred int64    `json:"bytes_transferred"`
				Files            []string `json:"files,omitempty"`
				Error            string   `json:"error,omitempty"`
			}

			err := client.Raw(cmd.Context(), "POST", "/api/v1/sessions/"+sessionID+"/sync", map[string]any{
				"mount_path": mountPath,
				"direction":  direction,
				"prefix":     prefix,
				"delete":     delete,
				"dry_run":    dryRun,
			}, &result)
			if err != nil {
				return err
			}

			if outputFmt == "json" {
				return printJSON(result)
			}

			if dryRun {
				fmt.Printf("Dry run: %s %s\n", result.Direction, result.MountPath)
				for _, f := range result.Files {
					fmt.Printf("  %s\n", f)
				}
				return nil
			}

			fmt.Printf("Synced %s (%s)\n", result.MountPath, result.Direction)
			fmt.Printf("  Added:   %d\n", result.FilesAdded)
			fmt.Printf("  Updated: %d\n", result.FilesUpdated)
			fmt.Printf("  Deleted: %d\n", result.FilesDeleted)
			if result.BytesTransferred > 0 {
				fmt.Printf("  Bytes:   %d\n", result.BytesTransferred)
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&direction, "direction", "d", "push", "Sync direction: push (VM→bucket) or pull (bucket→VM)")
	cmd.Flags().StringVar(&prefix, "prefix", "", "Limit sync to a sub-path within the mount")
	cmd.Flags().BoolVar(&delete, "delete", false, "Delete files in destination not in source")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview changes without syncing")
	return cmd
}
