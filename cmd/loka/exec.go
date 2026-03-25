package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/vyprai/loka/pkg/lokaapi"
)

func newExecCmd() *cobra.Command {
	var (
		workdir  string
		envVars  []string
		parallel bool
		cmds     []string
		wait     bool
	)

	cmd := &cobra.Command{
		Use:   "exec <session-id-or-name> [-- command args...]",
		Short: "Execute command(s) in a session",
		Long: `Execute a single command or multiple commands in parallel.
Accepts a session UUID or human-readable name.

Examples:
  loka exec brave-falcon-a3f2 -- echo hello
  loka exec <session-id> -- python script.py
  loka exec <session-id> --workdir /workspace -- npm test
  loka exec <session-id> --parallel --cmd "python analyze.py" --cmd "npm test"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			sessionID := args[0]

			// After cobra processes --, remaining args after session-id are the command.
			inlineCmd := args[1:]

			var req lokaapi.ExecReq

			if len(cmds) > 0 {
				// Parallel mode with --cmd flags.
				req.Parallel = parallel
				for _, c := range cmds {
					parts := strings.Fields(c)
					if len(parts) == 0 {
						continue
					}
					req.Commands = append(req.Commands, lokaapi.ExecCommand{
						Command: parts[0],
						Args:    parts[1:],
						Workdir: workdir,
					})
				}
			} else if len(inlineCmd) > 0 {
				// Single command after --.
				req.Command = inlineCmd[0]
				if len(inlineCmd) > 1 {
					req.Args = inlineCmd[1:]
				}
				req.Workdir = workdir
				if len(envVars) > 0 {
					req.Env = parseEnvVars(envVars)
				}
			} else {
				return fmt.Errorf("no command provided. Use '-- command' or '--cmd' flags")
			}

			exec, err := client.Exec(cmd.Context(), sessionID, req)
			if err != nil {
				return err
			}

			// Wait for completion if requested or by default.
			if wait || !parallel {
				for exec.Status == "pending" || exec.Status == "running" {
					time.Sleep(100 * time.Millisecond)
					updated, err := client.GetExecution(cmd.Context(), sessionID, exec.ID)
					if err != nil {
						break
					}
					exec = updated
				}
			}

			if outputFmt == "json" {
				return printJSON(exec)
			}

			// Print results.
			var results []struct {
				CommandID string `json:"CommandID"`
				ExitCode  int    `json:"ExitCode"`
				Stdout    string `json:"Stdout"`
				Stderr    string `json:"Stderr"`
			}
			if err := json.Unmarshal(exec.Results, &results); err == nil {
				for _, r := range results {
					if r.Stdout != "" {
						fmt.Print(r.Stdout)
					}
					if r.Stderr != "" {
						fmt.Fprint(cmd.ErrOrStderr(), r.Stderr)
					}
				}
			}

			// Set exit code from last command.
			if len(results) > 0 && results[len(results)-1].ExitCode != 0 {
				os.Exit(results[len(results)-1].ExitCode)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&workdir, "workdir", "", "Working directory")
	cmd.Flags().StringArrayVar(&envVars, "env", nil, "Environment variables (KEY=VALUE)")
	cmd.Flags().BoolVar(&parallel, "parallel", false, "Run commands in parallel")
	cmd.Flags().StringArrayVar(&cmds, "cmd", nil, "Command to run (for parallel execution)")
	cmd.Flags().BoolVar(&wait, "wait", true, "Wait for execution to complete")
	return cmd
}

func parseEnvVars(vars []string) map[string]string {
	env := make(map[string]string)
	for _, v := range vars {
		parts := strings.SplitN(v, "=", 2)
		if len(parts) == 2 {
			env[parts[0]] = parts[1]
		}
	}
	return env
}
