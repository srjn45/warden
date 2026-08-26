package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/router"
	"github.com/srjn45/warden/internal/store"
)

// runHotSwap executes the hot-swap against the session. It is a package var so
// tests can stub it or inject a hermetic test lifecycle.
var runHotSwap = func(ctx context.Context, cmd *cobra.Command, sess *store.Session, req lifecycle.SwapRequest) (*lifecycle.SwapResult, error) {
	cfg := config.Load(configPathFor(cmd))
	bstore, err := openBackendStore(cmd)
	if err != nil {
		return nil, fmt.Errorf("open backend store: %w", err)
	}
	defer bstore.Close()

	runner := lifecycle.HintingExecRunner{Inner: lifecycle.ExecRunner{}}
	lc := lifecycle.New(runner, cfg)
	lc.ProjectsDir = cfg.ClaudeProjectsDir
	lc.PromptsDir = filepath.Join(cfg.DataDir, "prompts")
	lc.HintsDir = filepath.Join(cfg.DataDir, "hints")
	lc.ExitsDir = filepath.Join(cfg.DataDir, "exits")
	lc.SettingsDir = filepath.Join(cfg.DataDir, "settings")
	lc.Resolver = router.NewResolver(bstore)

	res, err := lc.HotSwap(ctx, sess, req)
	if err != nil {
		return nil, err
	}

	// Persist mutated session
	st, err := store.NewFileStore(cfg.DataDir)
	if err == nil {
		defer st.Close(ctx)
		_ = st.Update(ctx, sess.ID, func(s *store.Session) error {
			s.Backend = sess.Backend
			s.Model = sess.Model
			s.ClaudeSessionID = sess.ClaudeSessionID
			s.UpdatedAt = sess.UpdatedAt
			return nil
		})
	}
	return res, nil
}

// resolveSwitchAgent resolves the target session for a switch: explicit argument,
// or WARDEN_SESSION_ID when invoked from inside an agent, or gitTarget.
func resolveSwitchAgent(cmd *cobra.Command, args []string) (*store.Session, error) {
	var id string
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		id = strings.TrimSpace(args[0])
	} else {
		_, session := gitTarget()
		id = session
	}
	if id == "" {
		return nil, fmt.Errorf("switch requires an agent-id argument or must be run inside a warden agent session (WARDEN_SESSION_ID is unset)")
	}

	// Look up session from daemon client or directly from store
	c := clientFor(cmd)
	sess, err := c.Get(cmd.Context(), id)
	if err == nil && sess != nil {
		return sess, nil
	}

	// Fallback to reading file store directly
	cfg := config.Load(configPathFor(cmd))
	st, err := store.NewFileStore(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("look up session %q: %w", id, err)
	}
	defer st.Close(cmd.Context())
	sess, err = st.Get(cmd.Context(), id)
	if err != nil {
		return nil, fmt.Errorf("look up session %q: %w", id, err)
	}
	return sess, nil
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
			"Examples:\n" +
			"  warden switch --backend antigravity --model gemini-3.1-pro\n" +
			"  warden switch --tier tier-1\n" +
			"  warden switch abc123 --tier tier-3 --prompt 'Focus on unit test coverage'\n",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sess, err := resolveSwitchAgent(cmd, args)
			if err != nil {
				return err
			}

			var modelTier backendstore.ModelTier
			if tier != "" {
				modelTier = backendstore.ModelTier(tier)
				if !modelTier.Valid() {
					return fmt.Errorf("invalid tier %q (valid: tier-1, tier-2, tier-3)", tier)
				}
			}

			swapReason := lifecycle.SwapReason(reason)
			if swapReason == "" {
				swapReason = lifecycle.SwapReasonManual
			}

			req := lifecycle.SwapRequest{
				Backend: backend,
				Model:   model,
				Tier:    modelTier,
				Role:    roleName,
				Reason:  swapReason,
				Prompt:  prompt,
			}

			res, err := runHotSwap(cmd.Context(), cmd, sess, req)
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
