package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newAlertsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "alerts",
		Short: "Manage alert rules and view alerts",
		Long: `Create, list, and manage alert rules. View active alerts and history.

Examples:
  loka alerts list
  loka alerts rules
  loka alerts rules create --name high-cpu --query "cpu_usage" --condition ">" --threshold 90 --for 5m --severity critical
  loka alerts dismiss <alert-id>
  loka alerts history --since 24h`,
	}

	cmd.AddCommand(
		newAlertsRulesCmd(),
		newAlertsListCmd(),
		newAlertsDismissCmd(),
		newAlertsHistoryCmd(),
	)
	return cmd
}

func newAlertsRulesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rules",
		Short: "List alert rules",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()

			var resp struct {
				Status string `json:"status"`
				Data   []struct {
					ID        string  `json:"id"`
					Name      string  `json:"name"`
					Query     string  `json:"query"`
					Condition string  `json:"condition"`
					Threshold float64 `json:"threshold"`
					For       string  `json:"for"`
					Severity  string  `json:"severity"`
					Enabled   bool    `json:"enabled"`
				} `json:"data"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/alerts/rules", nil, &resp); err != nil {
				return fmt.Errorf("list alert rules: %w", err)
			}

			if len(resp.Data) == 0 {
				fmt.Println("No alert rules configured. Create one with: loka alerts rules create")
				return nil
			}

			if outputFmt == "json" {
				data, _ := json.MarshalIndent(resp.Data, "", "  ")
				fmt.Println(string(data))
				return nil
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tQUERY\tCONDITION\tSEVERITY\tENABLED")
			for _, r := range resp.Data {
				cond := fmt.Sprintf("%s %.0f for %s", r.Condition, r.Threshold, r.For)
				enabled := "yes"
				if !r.Enabled {
					enabled = "no"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					short(r.ID), r.Name, r.Query, cond, r.Severity, enabled)
			}
			tw.Flush()
			return nil
		},
	}

	cmd.AddCommand(
		newAlertsRulesCreateCmd(),
		newAlertsRulesDeleteCmd(),
	)
	return cmd
}

func newAlertsRulesCreateCmd() *cobra.Command {
	var (
		name      string
		query     string
		condition string
		threshold float64
		forDur    string
		severity  string
		webhook   string
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new alert rule",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" || query == "" || condition == "" {
				return fmt.Errorf("--name, --query, and --condition are required")
			}

			body := map[string]interface{}{
				"name":      name,
				"query":     query,
				"condition": condition,
				"threshold": threshold,
				"for":       forDur,
				"severity":  severity,
				"enabled":   true,
			}
			if webhook != "" {
				body["webhooks"] = []string{webhook}
			}

			client := newClient()
			var resp struct {
				Status string                 `json:"status"`
				Data   map[string]interface{} `json:"data"`
			}
			if err := client.Raw(cmd.Context(), "POST", "/api/v1/alerts/rules", body, &resp); err != nil {
				return fmt.Errorf("create alert rule: %w", err)
			}

			if outputFmt == "json" {
				data, _ := json.MarshalIndent(resp.Data, "", "  ")
				fmt.Println(string(data))
				return nil
			}

			fmt.Printf("Alert rule %q created (id: %v)\n", name, resp.Data["id"])
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Rule name")
	cmd.Flags().StringVar(&query, "query", "", "PromQL-style query expression")
	cmd.Flags().StringVar(&condition, "condition", ">", "Comparison operator (>, <, >=, <=, ==, !=)")
	cmd.Flags().Float64Var(&threshold, "threshold", 0, "Threshold value")
	cmd.Flags().StringVar(&forDur, "for", "5m", "How long the condition must be true before firing")
	cmd.Flags().StringVar(&severity, "severity", "warning", "Alert severity: critical, warning, info")
	cmd.Flags().StringVar(&webhook, "webhook", "", "Webhook URL for notifications")

	return cmd
}

func newAlertsRulesDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete an alert rule",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			path := fmt.Sprintf("/api/v1/alerts/rules/%s", args[0])
			if err := client.Raw(cmd.Context(), "DELETE", path, nil, nil); err != nil {
				return fmt.Errorf("delete alert rule: %w", err)
			}
			fmt.Printf("Alert rule %q deleted\n", args[0])
			return nil
		},
	}
}

func newAlertsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List active alerts",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()

			var resp struct {
				Status string `json:"status"`
				Data   []struct {
					ID       string  `json:"id"`
					RuleName string  `json:"rule_name"`
					Status   string  `json:"status"`
					Severity string  `json:"severity"`
					Value    float64 `json:"value"`
					FiredAt  string  `json:"fired_at"`
				} `json:"data"`
			}
			if err := client.Raw(cmd.Context(), "GET", "/api/v1/alerts?status=firing", nil, &resp); err != nil {
				return fmt.Errorf("list alerts: %w", err)
			}

			if len(resp.Data) == 0 {
				fmt.Println("No active alerts.")
				return nil
			}

			if outputFmt == "json" {
				data, _ := json.MarshalIndent(resp.Data, "", "  ")
				fmt.Println(string(data))
				return nil
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tRULE\tSTATUS\tSEVERITY\tVALUE\tFIRED AT")
			for _, a := range resp.Data {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.2f\t%s\n",
					short(a.ID), a.RuleName, a.Status, a.Severity, a.Value, a.FiredAt)
			}
			tw.Flush()
			return nil
		},
	}
}

func newAlertsDismissCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dismiss <alert-id>",
		Short: "Dismiss a firing alert",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			path := fmt.Sprintf("/api/v1/alerts/%s/dismiss", args[0])
			body := map[string]string{"dismissed_by": "cli-user"}
			if err := client.Raw(cmd.Context(), "POST", path, body, nil); err != nil {
				return fmt.Errorf("dismiss alert: %w", err)
			}
			fmt.Printf("Alert %q dismissed\n", args[0])
			return nil
		},
	}
}

func newAlertsHistoryCmd() *cobra.Command {
	var since string

	cmd := &cobra.Command{
		Use:   "history",
		Short: "Show alert history",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()

			path := "/api/v1/alerts/history"
			if since != "" {
				path += "?since=" + since
			}

			var resp struct {
				Status string `json:"status"`
				Data   []struct {
					ID         string  `json:"id"`
					RuleName   string  `json:"rule_name"`
					Status     string  `json:"status"`
					Severity   string  `json:"severity"`
					Value      float64 `json:"value"`
					FiredAt    string  `json:"fired_at"`
					ResolvedAt string  `json:"resolved_at,omitempty"`
				} `json:"data"`
			}
			if err := client.Raw(cmd.Context(), "GET", path, nil, &resp); err != nil {
				return fmt.Errorf("alert history: %w", err)
			}

			if len(resp.Data) == 0 {
				fmt.Println("No alert history found.")
				return nil
			}

			if outputFmt == "json" {
				data, _ := json.MarshalIndent(resp.Data, "", "  ")
				fmt.Println(string(data))
				return nil
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tRULE\tSTATUS\tSEVERITY\tVALUE\tFIRED\tRESOLVED")
			for _, a := range resp.Data {
				resolved := "-"
				if a.ResolvedAt != "" {
					resolved = a.ResolvedAt
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.2f\t%s\t%s\n",
					short(a.ID), a.RuleName, a.Status, a.Severity, a.Value, a.FiredAt, resolved)
			}
			tw.Flush()
			return nil
		},
	}

	cmd.Flags().StringVar(&since, "since", "24h", "Show alerts since duration (e.g. 24h, 7d)")
	return cmd
}

// short truncates a UUID to the first 8 characters for display.
func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
