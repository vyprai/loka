package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/rizqme/loka/pkg/lokaapi"
)

func newCheckpointCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "checkpoint",
		Short:   "Manage checkpoints",
		Aliases: []string{"cp"},
	}
	cmd.AddCommand(
		newCheckpointCreateCmd(),
		newCheckpointListCmd(),
		newCheckpointTreeCmd(),
		newCheckpointRestoreCmd(),
		newCheckpointDiffCmd(),
	)
	return cmd
}

func newCheckpointCreateCmd() *cobra.Command {
	var (
		cpType string
		label  string
	)

	cmd := &cobra.Command{
		Use:   "create <session-id>",
		Short: "Create a checkpoint",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			cp, err := client.CreateCheckpoint(cmd.Context(), args[0], lokaapi.CreateCheckpointReq{
				Type:  cpType,
				Label: label,
			})
			if err != nil {
				return err
			}
			if outputFmt == "json" {
				return printJSON(cp)
			}
			fmt.Printf("Checkpoint created: %s\n", shortID(cp.ID))
			fmt.Printf("  Type:   %s\n", cp.Type)
			fmt.Printf("  Label:  %s\n", cp.Label)
			fmt.Printf("  Parent: %s\n", shortID(cp.ParentID))
			return nil
		},
	}

	cmd.Flags().StringVar(&cpType, "type", "light", "Checkpoint type: light or full")
	cmd.Flags().StringVar(&label, "label", "", "Optional label")
	return cmd
}

func newCheckpointListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <session-id>",
		Short: "List checkpoints for a session",
		Aliases: []string{"ls"},
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			resp, err := client.ListCheckpoints(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if outputFmt == "json" {
				return printJSON(resp)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tPARENT\tTYPE\tSTATUS\tLABEL\tCREATED")
			for _, cp := range resp.Checkpoints {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					shortID(cp.ID), shortID(cp.ParentID), cp.Type, cp.Status,
					cp.Label, cp.CreatedAt.Format("2006-01-02 15:04:05"))
			}
			w.Flush()
			return nil
		},
	}
}

func newCheckpointTreeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tree <session-id>",
		Short: "Show checkpoint DAG as a tree",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			resp, err := client.ListCheckpoints(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if len(resp.Checkpoints) == 0 {
				fmt.Println("No checkpoints")
				return nil
			}

			// Build parent -> children map.
			children := make(map[string][]lokaapi.Checkpoint)
			var roots []lokaapi.Checkpoint
			for _, cp := range resp.Checkpoints {
				if cp.ParentID == "" {
					roots = append(roots, cp)
				} else {
					children[cp.ParentID] = append(children[cp.ParentID], cp)
				}
			}

			// Print tree.
			for i, root := range roots {
				isLast := i == len(roots)-1
				printTreeNode(root, resp.Current, children, "", isLast)
			}
			return nil
		},
	}
}

func printTreeNode(cp lokaapi.Checkpoint, currentID string, children map[string][]lokaapi.Checkpoint, prefix string, isLast bool) {
	connector := "├── "
	if isLast {
		connector = "└── "
	}

	label := ""
	if cp.Label != "" {
		label = fmt.Sprintf(" %q", cp.Label)
	}
	current := ""
	if cp.ID == currentID {
		current = " [current]"
	}

	fmt.Printf("%s%s%s (%s, %s)%s%s\n",
		prefix, connector, shortID(cp.ID), cp.Type,
		cp.CreatedAt.Format("15:04:05"),
		label, current)

	childPrefix := prefix
	if isLast {
		childPrefix += "    "
	} else {
		childPrefix += "│   "
	}

	kids := children[cp.ID]
	for i, child := range kids {
		printTreeNode(child, currentID, children, childPrefix, i == len(kids)-1)
	}
}

func newCheckpointRestoreCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restore <session-id> <checkpoint-id>",
		Short: "Restore session to a checkpoint",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			sess, err := client.RestoreCheckpoint(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			_ = sess
			fmt.Printf("Restored session %s to checkpoint %s\n",
				shortID(args[0]), shortID(args[1]))
			return nil
		},
	}
}

func newCheckpointDiffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diff <session-id> <checkpoint-a> <checkpoint-b>",
		Short: "Show differences between two checkpoints",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			sessionID, cpA, cpB := args[0], args[1], args[2]

			var resp struct {
				SessionID   string `json:"session_id"`
				CheckpointA string `json:"checkpoint_a"`
				CheckpointB string `json:"checkpoint_b"`
				LabelA      string `json:"label_a"`
				LabelB      string `json:"label_b"`
				TypeA       string `json:"type_a"`
				TypeB       string `json:"type_b"`
			}
			path := fmt.Sprintf("/api/v1/sessions/%s/checkpoints/diff?a=%s&b=%s", sessionID, cpA, cpB)
			if err := client.Raw(cmd.Context(), "GET", path, nil, &resp); err != nil {
				return err
			}

			if outputFmt == "json" {
				return printJSON(resp)
			}

			labelA := resp.LabelA
			if labelA == "" {
				labelA = shortID(cpA)
			}
			labelB := resp.LabelB
			if labelB == "" {
				labelB = shortID(cpB)
			}

			fmt.Printf("Diff: %s (%s) -> %s (%s)\n", labelA, resp.TypeA, labelB, resp.TypeB)
			fmt.Printf("Session: %s\n", shortID(sessionID))
			return nil
		},
	}
}

var _ = strings.Join
