package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func newTaskCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "task",
		Short: "Run and manage one-time tasks",
	}
	cmd.AddCommand(
		newTaskRunCmd(),
		newTaskListCmd(),
		newTaskLogsCmd(),
		newTaskRestartCmd(),
		newTaskCancelCmd(),
		newTaskDeleteCmd(),
	)
	return cmd
}

func newTaskRunCmd() *cobra.Command {
	var (
		name    string
		envVars []string
		wait    bool
		timeout int
	)

	cmd := &cobra.Command{
		Use:   "run <image> [-- command args...]",
		Short: "Run a one-time task",
		Long: `Run a command in a microVM. The task exits when the command completes.

Examples:
  loka task run python:3.12 -- python3 train.py
  loka task run --name migrate postgres:15 -- psql -c "ALTER TABLE..."
  loka task run alpine -- echo hello`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			imageRef := args[0]
			taskCmd := strings.Join(args[1:], " ")

			if taskCmd == "" {
				return fmt.Errorf("command is required after image (use -- to separate)")
			}

			// Parse env vars.
			env := make(map[string]string)
			for _, e := range envVars {
				parts := strings.SplitN(e, "=", 2)
				if len(parts) == 2 {
					env[parts[0]] = parts[1]
				}
			}

			if name == "" {
				name = imageRef
				if i := strings.LastIndex(name, "/"); i >= 0 {
					name = name[i+1:]
				}
				if i := strings.Index(name, ":"); i >= 0 {
					name = name[:i]
				}
			}

			sp := startSpinner(fmt.Sprintf("Running task: %s", name))
			client := newClient()

			req := map[string]any{
				"name":    name,
				"image":   imageRef,
				"command": taskCmd,
				"env":     env,
			}
			if timeout > 0 {
				req["timeout"] = timeout
			}

			var task struct {
				ID            string `json:"ID"`
				Name          string `json:"Name"`
				Status        string `json:"Status"`
				ExitCode      int    `json:"ExitCode"`
				StatusMessage string `json:"StatusMessage"`
			}
			if err := client.Raw(cmd.Context(), "POST", "/api/v1/tasks", req, &task); err != nil {
				sp.fail("Failed")
				return fmt.Errorf("run task: %w", err)
			}
			sp.stop(fmt.Sprintf("Task %s created", task.Name))

			if !wait {
				fmt.Printf("  ID: %s\n", shortID(task.ID))
				return nil
			}

			sp = startSpinner("Running")
			for i := 0; i < 3600; i++ {
				time.Sleep(1 * time.Second)
				var updated struct {
					ID            string `json:"ID"`
					Status        string `json:"Status"`
					ExitCode      int    `json:"ExitCode"`
					StatusMessage string `json:"StatusMessage"`
				}
				if err := client.Raw(cmd.Context(), "GET", "/api/v1/tasks/"+task.ID, nil, &updated); err != nil {
					continue
				}
				if updated.StatusMessage != "" {
					sp.update(updated.StatusMessage)
				}
				switch updated.Status {
				case "success":
					sp.stop(fmt.Sprintf("Success (exit 0)"))
					return nil
				case "failed":
					sp.fail(fmt.Sprintf("Failed (exit %d)", updated.ExitCode))
					return fmt.Errorf("task failed with exit code %d", updated.ExitCode)
				case "error":
					sp.fail(updated.StatusMessage)
					return fmt.Errorf("task error: %s", updated.StatusMessage)
				}
			}
			sp.fail("Timeout")
			return fmt.Errorf("task did not complete within timeout")
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Task name")
	cmd.Flags().StringArrayVar(&envVars, "env", nil, "Environment variable (KEY=VALUE)")
	cmd.Flags().BoolVar(&wait, "wait", true, "Wait for task to complete")
	cmd.Flags().IntVar(&timeout, "timeout", 0, "Max duration in seconds (0 = no limit)")
	return cmd
}

func newTaskListCmd() *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List tasks", Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			var resp struct {
				Tasks []struct {
					ID          string    `json:"ID"`
					Name        string    `json:"Name"`
					Status      string    `json:"Status"`
					ExitCode    int       `json:"ExitCode"`
					ImageRef    string    `json:"ImageRef"`
					StartedAt   time.Time `json:"StartedAt"`
					CompletedAt time.Time `json:"CompletedAt"`
					CreatedAt   time.Time `json:"CreatedAt"`
				} `json:"tasks"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/tasks", nil, &resp); err != nil {
				return err
			}
			if outputFmt == "json" {
				return printJSON(resp)
			}
			if len(resp.Tasks) == 0 {
				fmt.Println("No tasks. Run one: loka task run alpine -- echo hello")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSTATUS\tEXIT\tIMAGE\tDURATION\tCREATED")
			for _, t := range resp.Tasks {
				dur := "-"
				if !t.StartedAt.IsZero() {
					end := t.CompletedAt
					if end.IsZero() {
						end = time.Now()
					}
					dur = end.Sub(t.StartedAt).Truncate(time.Second).String()
				}
				exitStr := "-"
				if t.Status == "success" || t.Status == "failed" {
					exitStr = fmt.Sprintf("%d", t.ExitCode)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					t.Name, t.Status, exitStr, truncate(t.ImageRef, 25),
					dur, t.CreatedAt.Format("2006-01-02 15:04"))
			}
			w.Flush()
			return nil
		},
	}
}

func newTaskLogsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "logs <name-or-id>", Short: "View task output", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			var resp struct {
				Stdout []string `json:"stdout"`
				Stderr []string `json:"stderr"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/tasks/"+args[0]+"/logs", nil, &resp); err != nil {
				return err
			}
			for _, line := range resp.Stdout {
				fmt.Println(line)
			}
			for _, line := range resp.Stderr {
				fmt.Fprintf(os.Stderr, "%s\n", line)
			}
			return nil
		},
	}
}

func newTaskRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use: "restart <name-or-id>", Short: "Restart a completed/failed task", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			var task struct {
				ID   string `json:"ID"`
				Name string `json:"Name"`
			}
			if err := client.Raw(cmd.Context(), "POST", "/api/v1/tasks/"+args[0]+"/restart", nil, &task); err != nil {
				return err
			}
			fmt.Printf("Task %s restarted (new ID: %s)\n", task.Name, shortID(task.ID))
			return nil
		},
	}
}

func newTaskCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use: "cancel <name-or-id>", Short: "Cancel a running task", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			if err := client.Raw(cmd.Context(), "POST", "/api/v1/tasks/"+args[0]+"/cancel", nil, nil); err != nil {
				return err
			}
			fmt.Printf("Task %s cancelled\n", args[0])
			return nil
		},
	}
}

func newTaskDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use: "rm <name-or-id>", Short: "Delete a task record", Aliases: []string{"delete"},
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			if err := client.Raw(cmd.Context(), "DELETE", "/api/v1/tasks/"+args[0], nil, nil); err != nil {
				return err
			}
			fmt.Printf("Task %s deleted\n", args[0])
			return nil
		},
	}
}
