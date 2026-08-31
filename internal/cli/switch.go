package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/lifecycle"
)

// runHotSwap performs the hot-swap through the daemon's switch endpoint. It is a
// package var so tests can stub it. The daemon is the SOLE owner of the writable
// session store: `warden switch` no longer opens a second FileStore to run the
// swap and persist the mutation (that second writer is exactly the corruption
// source this migration closes) — it asks the daemon, which performs the
// lifecycle hot-swap, serializes the session update through its own store, and
// publishes the change notification.
var runHotSwap = func(ctx context.Context, cmd *cobra.Command, id string, params client.SwitchSessionParams) (*lifecycle.SwapResult, error) {
	res, err := clientFor(cmd).SwitchSession(ctx, id, params)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// resolveSwitchTarget resolves the target session id for a switch: an explicit
// argument, or WARDEN_SESSION_ID when invoked from inside an agent, or the
// git-inferred session. It resolves an id string only — the daemon's switch
// endpoint looks the session up by name-or-id itself, so the CLI never opens the
// live store to do it (no direct-store fallback, per single-owner enforcement).
func resolveSwitchTarget(cmd *cobra.Command, args []string) (string, error) {
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		return strings.TrimSpace(args[0]), nil
	}
	if _, session := gitTarget(); session != "" {
		return session, nil
	}
	return "", fmt.Errorf("switch requires an agent-id argument or must be run inside a warden agent session (WARDEN_SESSION_ID is unset)")
}

func newSwitchCmd() *cobra.Command {
	var backend string
	var model string
	var tier string
	var roleName string
	var prompt string
	var reason string
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "switch [agent-id]",
		Short: "Hot-swap an agent session to a different backend, model, or tier mid-task",
		Long: "Mid-session hot-swap: retire the active CLI process and launch a successor backend\n" +
			"in the SAME worktree, carrying forward structured context (Goal, Decisions Log,\n" +
			"Modified Files Diff, Immediate Next Step) so the new agent continues without starting cold.\n\n" +
			"The successor can be chosen by explicit --backend and/or --model, or by --tier\n" +
			"(resolved via quota-balanced weighted headroom routing across eligible backends).\n\n" +
			"The swap is performed by the warden daemon (the sole owner of the session store),\n" +
			"so the daemon must be running.\n\n" +
			"Examples:\n" +
			"  warden switch --backend antigravity --model gemini-3.1-pro\n" +
			"  warden switch --tier tier-1\n" +
			"  warden switch abc123 --tier tier-3 --prompt 'Focus on unit test coverage'\n",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := resolveSwitchTarget(cmd, args)
			if err != nil {
				return err
			}

			if tier != "" {
				if modelTier := backendstore.ModelTier(tier); !modelTier.Valid() {
					return fmt.Errorf("invalid tier %q (valid: tier-1, tier-2, tier-3)", tier)
				}
			}

			params := client.SwitchSessionParams{
				Backend: backend,
				Model:   model,
				Tier:    tier,
				Role:    roleName,
				Reason:  reason,
				Prompt:  prompt,
			}

			res, err := runHotSwap(cmd.Context(), cmd, id, params)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}

			fromDesc := formatBackendDesc(res.FromBackend, res.FromModel)
			toDesc := formatBackendDesc(res.ToBackend, res.ToModel)
			fmt.Fprintf(out, "switched agent %s: %s → %s\nhandoff written to %s\n",
				res.Session.ID, fromDesc, toDesc, res.HandoffPath)
			return nil
		},
	}

	cmd.Flags().StringVarP(&backend, "backend", "b", "", "explicit successor backend id (claude, antigravity, codex, …)")
	cmd.Flags().StringVarP(&model, "model", "m", "", "explicit successor model id")
	cmd.Flags().StringVarP(&tier, "tier", "t", "", "resolve successor via quota-balanced router at this tier (tier-1|tier-2|tier-3)")
	cmd.Flags().StringVarP(&roleName, "role", "r", "", "role to resolve tier from when --tier is not given")
	cmd.Flags().StringVarP(&prompt, "prompt", "p", "", "optional extra instruction appended to successor's continuation prompt")
	cmd.Flags().StringVar(&reason, "reason", "manual", "reason recorded for hot-swap (manual|context_fill|quota)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit result as JSON")
	return cmd
}

func formatBackendDesc(backend, model string) string {
	if backend == "" {
		backend = "unknown"
	}
	if model == "" {
		return backend
	}
	return backend + " (" + model + ")"
}
