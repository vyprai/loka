package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/vyprai/loka/internal/loka/logs"
)

func newLogsCmd() *cobra.Command {
	var (
		queryRange string
		limit      int
		jsonOut    bool
	)

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Query and explore logs with a TUI viewer",
		Long: `Interactive TUI for querying Loki-compatible logs.

Without arguments, launches an interactive log viewer. With a query argument
and --json, prints results to stdout for scripting.

Examples:
  loka logs                                                # launch TUI
  loka logs query '{type="service"} |= "error"' --json     # one-shot query
  loka logs tail svc_xyz                                    # live tail`,
		RunE: func(cmd *cobra.Command, args []string) error {
			endpoint, tok, _, _ := resolveServer()
			lc := logs.NewClient(endpoint, tok)

			// Interactive TUI.
			m := logs.NewModel(lc)
			p := tea.NewProgram(m, tea.WithAltScreen())
			if _, err := p.Run(); err != nil {
				return fmt.Errorf("TUI error: %w", err)
			}
			return nil
		},
	}

	// Subcommand: query
	queryCmd := &cobra.Command{
		Use:   "query [logql-query]",
		Short: "Execute a one-shot LogQL query",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			endpoint, tok, _, _ := resolveServer()
			lc := logs.NewClient(endpoint, tok)
			ctx := cmd.Context()

			query := args[0]

			if queryRange != "" {
				dur, err := time.ParseDuration(queryRange)
				if err != nil {
					return fmt.Errorf("invalid --range %q: %w", queryRange, err)
				}
				now := time.Now()
				start := fmt.Sprintf("%d", now.Add(-dur).UnixNano())
				end := fmt.Sprintf("%d", now.UnixNano())

				resp, err := lc.QueryRange(ctx, query, start, end, limit)
				if err != nil {
					return fmt.Errorf("query_range: %w", err)
				}
				return printLogsOutput(resp, jsonOut)
			}

			resp, err := lc.Query(ctx, query, limit)
			if err != nil {
				return fmt.Errorf("query: %w", err)
			}
			return printLogsOutput(resp, jsonOut)
		},
	}
	queryCmd.Flags().StringVar(&queryRange, "range", "1h", "Time range (e.g. 1h, 24h)")
	queryCmd.Flags().IntVar(&limit, "limit", 100, "Maximum number of entries")
	queryCmd.Flags().BoolVar(&jsonOut, "json", false, "Output results as JSON")

	// Subcommand: tail
	tailCmd := &cobra.Command{
		Use:   "tail [service-id-or-query]",
		Short: "Live tail log entries (polls every 2s)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			endpoint, tok, _, _ := resolveServer()
			lc := logs.NewClient(endpoint, tok)
			ctx := cmd.Context()

			input := args[0]
			// If the input doesn't look like a LogQL query, wrap it.
			query := input
			if !strings.HasPrefix(input, "{") {
				query = fmt.Sprintf(`{id="%s"}`, input)
			}

			seen := make(map[string]struct{})
			for {
				select {
				case <-ctx.Done():
					return nil
				default:
				}

				resp, err := lc.Tail(ctx, query, limit)
				if err != nil {
					fmt.Fprintf(os.Stderr, "tail error: %v\n", err)
					time.Sleep(2 * time.Second)
					continue
				}

				printTailEntries(resp, seen, jsonOut)
				time.Sleep(2 * time.Second)
			}
		},
	}
	tailCmd.Flags().IntVar(&limit, "limit", 100, "Maximum entries per poll")
	tailCmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")

	cmd.AddCommand(queryCmd, tailCmd)

	return cmd
}

// printLogsOutput prints a query response either as JSON or formatted text.
func printLogsOutput(resp *logs.QueryResponse, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}

	// Text output: parse streams and print line by line.
	var streams []logs.StreamResult
	if err := json.Unmarshal(resp.Data.Result, &streams); err != nil {
		return fmt.Errorf("decode streams: %w", err)
	}

	for _, s := range streams {
		for _, v := range s.Values {
			tsNs := v[0]
			msg := v[1]

			var nsec int64
			fmt.Sscanf(tsNs, "%d", &nsec)
			ts := time.Unix(0, nsec)

			level := s.Stream["level"]
			name := s.Stream["name"]
			if name == "" {
				name = s.Stream["id"]
			}

			fmt.Printf("%s  %-5s  [%s]  %s\n",
				ts.Format("15:04:05.000"),
				strings.ToUpper(level),
				name,
				msg,
			)
		}
	}
	return nil
}

// printTailEntries prints new entries from a tail response, deduplicating by timestamp+message.
func printTailEntries(resp *logs.QueryResponse, seen map[string]struct{}, asJSON bool) {
	var streams []logs.StreamResult
	if err := json.Unmarshal(resp.Data.Result, &streams); err != nil {
		return
	}

	for _, s := range streams {
		for _, v := range s.Values {
			key := v[0] + "|" + v[1]
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}

			if asJSON {
				entry := map[string]interface{}{
					"ts":     v[0],
					"msg":    v[1],
					"stream": s.Stream,
				}
				data, _ := json.Marshal(entry)
				fmt.Println(string(data))
			} else {
				var nsec int64
				fmt.Sscanf(v[0], "%d", &nsec)
				ts := time.Unix(0, nsec)

				level := s.Stream["level"]
				name := s.Stream["name"]
				if name == "" {
					name = s.Stream["id"]
				}

				fmt.Printf("%s  %-5s  [%s]  %s\n",
					ts.Format("15:04:05.000"),
					strings.ToUpper(level),
					name,
					v[1],
				)
			}
		}
	}
}
