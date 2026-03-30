package main

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func newInstanceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "instance",
		Short: "Manage all running instances (sessions and services)",
	}
	cmd.AddCommand(
		newInstanceListCmd(),
		newInstanceDestroyCmd(),
	)
	return cmd
}

type instanceRow struct {
	Type    string
	Name    string
	ID      string
	Status  string
	Image   string
	Port    string
	Created time.Time
}

func newInstanceListCmd() *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List all active instances", Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			var rows []instanceRow

			// Fetch sessions.
			sessions, err := client.ListSessions(cmd.Context())
			if err == nil {
				for _, s := range sessions.Sessions {
					name := s.Name
					if name == "" {
						name = shortID(s.ID)
					}
					img := s.ImageRef
					if img == "" {
						img = "-"
					} else if len(img) > 30 {
						img = img[:30]
					}
					rows = append(rows, instanceRow{
						Type:    "session",
						Name:    name,
						ID:      s.ID,
						Status:  s.Status,
						Image:   img,
						Port:    "-",
						Created: s.CreatedAt,
					})
				}
			}

			// Fetch services (including databases via type=all).
			var svcResp struct {
				Services []struct {
					ID             string `json:"ID"`
					Name           string `json:"Name"`
					Status         string `json:"Status"`
					ImageRef       string `json:"ImageRef"`
					Port           int    `json:"Port"`
					Ready          bool   `json:"Ready"`
					DatabaseConfig *struct {
						Engine string `json:"engine"`
					} `json:"DatabaseConfig"`
					CreatedAt time.Time `json:"CreatedAt"`
				} `json:"services"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/services?type=all", nil, &svcResp); err == nil {
				for _, s := range svcResp.Services {
					name := s.Name
					if name == "" {
						name = shortID(s.ID)
					}
					status := s.Status
					if s.Ready {
						status += " (ready)"
					}
					img := s.ImageRef
					if img == "" {
						img = "-"
					} else if len(img) > 30 {
						img = img[:30]
					}
					portStr := "-"
					if s.Port > 0 {
						portStr = fmt.Sprintf("%d", s.Port)
					}
					rowType := "service"
					if s.DatabaseConfig != nil {
						rowType = "database"
					}
					rows = append(rows, instanceRow{
						Type:    rowType,
						Name:    name,
						ID:      s.ID,
						Status:  status,
						Image:   img,
						Port:    portStr,
						Created: s.CreatedAt,
					})
				}
			}

			// Fetch tasks.
			var taskResp struct {
				Tasks []struct {
					ID        string    `json:"ID"`
					Name      string    `json:"Name"`
					Status    string    `json:"Status"`
					ExitCode  int       `json:"ExitCode"`
					ImageRef  string    `json:"ImageRef"`
					CreatedAt time.Time `json:"CreatedAt"`
				} `json:"tasks"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/tasks", nil, &taskResp); err == nil {
				for _, t := range taskResp.Tasks {
					name := t.Name
					if name == "" {
						name = shortID(t.ID)
					}
					status := t.Status
					if status == "failed" {
						status = fmt.Sprintf("failed (exit %d)", t.ExitCode)
					}
					img := t.ImageRef
					if len(img) > 30 {
						img = img[:30]
					}
					rows = append(rows, instanceRow{
						Type:    "task",
						Name:    name,
						ID:      t.ID,
						Status:  status,
						Image:   img,
						Port:    "-",
						Created: t.CreatedAt,
					})
				}
			}

			if outputFmt == "json" {
				return printJSON(rows)
			}

			if len(rows) == 0 {
				fmt.Println("No instances running.")
				return nil
			}

			// Sort newest first.
			sort.Slice(rows, func(i, j int) bool {
				return rows[i].Created.After(rows[j].Created)
			})

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "TYPE\tNAME\tSTATUS\tIMAGE\tPORT\tCREATED")
			for _, r := range rows {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					r.Type, r.Name, r.Status, r.Image, r.Port,
					r.Created.Format("2006-01-02 15:04"))
			}
			w.Flush()
			return nil
		},
	}
}

func newInstanceDestroyCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "destroy <name-or-id>",
		Short:   "Stop and destroy an instance (session or service)",
		Aliases: []string{"rm", "delete"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			nameOrID := args[0]

			// Try session first.
			sess, err := client.GetSession(cmd.Context(), nameOrID)
			if err == nil && sess != nil {
				client.DestroySession(cmd.Context(), sess.ID)
				name := sess.Name
				if name == "" {
					name = shortID(sess.ID)
				}
				fmt.Printf("Session %s destroyed\n", name)
				return nil
			}

			// Try service.
			var svc struct {
				ID   string `json:"ID"`
				Name string `json:"Name"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/services/"+nameOrID, nil, &svc); err == nil {
				client.Raw(cmd.Context(), "POST", "/api/v1/services/"+svc.ID+"/stop", nil, nil)
				client.Raw(cmd.Context(), "DELETE", "/api/v1/services/"+svc.ID, nil, nil)
				name := svc.Name
				if name == "" {
					name = shortID(svc.ID)
				}
				fmt.Printf("Service %s destroyed\n", name)
				return nil
			}

			return fmt.Errorf("instance %q not found", nameOrID)
		},
	}
}
