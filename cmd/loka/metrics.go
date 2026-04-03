package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	lokametrics "github.com/vyprai/loka/internal/loka/metrics"
)

func newMetricsCmd() *cobra.Command {
	var (
		query      string
		queryRange string
		step       string
		jsonOut    bool
	)

	cmd := &cobra.Command{
		Use:   "metrics",
		Short: "Query and explore metrics with a TUI dashboard",
		Long: `Interactive TUI for querying Prometheus-compatible metrics.

Without --query, launches an interactive dashboard. With --query and --json,
prints results to stdout for scripting.

Examples:
  loka metrics                                       # launch TUI
  loka metrics --query 'cpu{type="service"}' --json  # instant query, JSON output
  loka metrics --query 'cpu' --range 1h --step 1m --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			endpoint, tok, _, _ := resolveServer()
			mc := lokametrics.NewClient(endpoint, tok)

			// Non-interactive mode.
			if query != "" && jsonOut {
				return runNonInteractive(cmd, mc, query, queryRange, step)
			}

			// Interactive TUI.
			m := lokametrics.NewModel(mc)
			p := tea.NewProgram(m, tea.WithAltScreen())
			if _, err := p.Run(); err != nil {
				return fmt.Errorf("TUI error: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&query, "query", "", "PromQL query (non-interactive mode)")
	cmd.Flags().StringVar(&queryRange, "range", "", "Time range (e.g. 1h, 24h) for range queries")
	cmd.Flags().StringVar(&step, "step", "1m", "Step interval for range queries")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output results as JSON")

	return cmd
}

func runNonInteractive(cmd *cobra.Command, mc *lokametrics.MetricsClient, query, queryRange, step string) error {
	ctx := cmd.Context()

	if queryRange != "" {
		// Range query.
		dur, err := time.ParseDuration(queryRange)
		if err != nil {
			return fmt.Errorf("invalid --range %q: %w", queryRange, err)
		}
		now := time.Now()
		start := fmt.Sprintf("%d", now.Add(-dur).Unix())
		end := fmt.Sprintf("%d", now.Unix())

		resp, err := mc.QueryRange(ctx, query, start, end, step)
		if err != nil {
			return fmt.Errorf("range query: %w", err)
		}
		return printMetricsJSON(resp)
	}

	// Instant query.
	resp, err := mc.Query(ctx, query, nil)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	return printMetricsJSON(resp)
}

func printMetricsJSON(v interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
