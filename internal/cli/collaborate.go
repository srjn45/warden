package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/client"
)

func newCollaborateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collaborate",
		Short: "Collaboration groups: join or leave a named set of per-project orchestrators",
		Long: "Manage collaboration groups — named sets of per-project orchestrator agents\n" +
			"that become mutually discoverable so they can message and delegate across projects.\n\n" +
			"Join a group to seat this agent as its project's orchestrator; warden brokers\n" +
			"introductions to existing members. Leave to remove this agent's seat (soft:\n" +
			"in-flight messages still deliver; no new inbound work is accepted).",
	}
	cmd.PersistentFlags().String("as", "", "act as this agent id (defaults to $WARDEN_SESSION_ID)")
	cmd.AddCommand(newCollaborateGroupCmd())
	return cmd
}

func newCollaborateGroupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "group <name> (join|leave)",
		Short: "Join or leave a collaboration group",
		Long: "Join or leave a named collaboration group.\n\n" +
			"join: seat this agent in <name> as its project's orchestrator, creating the group\n" +
			"if it does not exist. Warden enforces one orchestrator per project (duplicate join\n" +
			"returns the already-seated agent id), switches this agent to the orchestrator role,\n" +
			"resolves its project summary, and brokers introductions both directions.\n\n" +
			"leave: remove this agent's seat from <name> and notify peers. Soft — in-flight\n" +
			"replies still deliver; only new inbound delegations are stopped.",
		Args:    cobra.ExactArgs(2),
		Example: "  wd collaborate group my-team join\n  wd collaborate group my-team leave",
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			action := strings.ToLower(args[1])

			as, _ := cmd.Flags().GetString("as")
			agentID, err := resolveSelf(as, envID("SESSION_ID"))
			if err != nil {
				return err
			}

			cl := clientFor(cmd)
			out := cmd.OutOrStdout()

			switch action {
			case "join":
				res, err := cl.JoinGroup(cmd.Context(), name, agentID)
				if err != nil {
					var se *client.StatusError
					if errors.As(err, &se) && se.Code == 409 {
						return fmt.Errorf("group %q already has an orchestrator for this project: %s", name, string(se.Body))
					}
					return err
				}
				fmt.Fprintf(out, "joined group %q as %s (role: %s)\n", name, agentID, res.Role)
				printGroupRoster(cmd, res.Group)
				return nil
			case "leave":
				res, err := cl.LeaveGroup(cmd.Context(), name, agentID)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "left group %q\n", name)
				printGroupRoster(cmd, res)
				return nil
			default:
				return fmt.Errorf("unknown action %q: want join or leave", action)
			}
		},
	}
}

func printGroupRoster(cmd *cobra.Command, g client.Group) {
	out := cmd.OutOrStdout()
	if len(g.Members) == 0 {
		fmt.Fprintln(out, "  (empty roster)")
		return
	}
	fmt.Fprintf(out, "  roster (%d member(s)):\n", len(g.Members))
	for _, m := range g.Members {
		line := fmt.Sprintf("    %s  project:%s", m.AgentID, m.ProjectKey)
		if m.Summary != "" {
			line += "  — " + m.Summary
		}
		fmt.Fprintln(out, line)
	}
}
