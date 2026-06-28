package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/agentbackend"
	_ "github.com/srjn45/warden/internal/agentbackend/backends" // register the adapters so agentbackend.Get resolves in-process
)

// modelsBackend resolves the agent.Backend this `wd models` runs for, mirroring
// reviewBackend: an explicit --backend wins (list a chosen backend, or run outside an
// agent); otherwise the owning agent's WARDEN_SESSION_ID is looked up (an existing read
// — no new daemon surface) and its recorded backend is used; failing both it falls
// through to the default backend, which surfaces a clear "no live model menu" degrade.
// It is a package var so tests can stub the session lookup without a live daemon.
var modelsBackend = func(cmd *cobra.Command, session, override string) (agentbackend.Backend, error) {
	id := override
	if id == "" && session != "" {
		if s, err := clientFor(cmd).Get(context.Background(), session); err == nil && s != nil {
			id = s.Backend
		}
	}
	return agentbackend.Get(id)
}

func newModelsCmd() *cobra.Command {
	var backend string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "models",
		Short: "List the agent backend's live model menu",
		Long: "Show the live, currently-available model menu the agent's backend exposes — the\n" +
			"agent-native counterpart to warden's static model aliases. Backends whose model\n" +
			"set is a runtime, multi-vendor menu (Antigravity: `agy models`, listing Gemini,\n" +
			"Claude, and GPT-OSS variants) report exactly which ids they will accept on\n" +
			"--model right now, so you don't have to guess from a hard-coded table.\n\n" +
			"Listing is a metadata read — it runs the backend's own list subcommand, not a\n" +
			"generation request — so it does not spend a hosted backend's request quota. The\n" +
			"menu prints one id per line; pass --json for a JSON array instead.\n\n" +
			"Backends with a static model set (e.g. Claude) have no live menu and are not\n" +
			"offered the verb — it exits non-zero pointing you at `--model` with a known id.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, session := gitTarget()
			be, err := modelsBackend(cmd, session, backend)
			if err != nil {
				return err
			}
			ml, ok := be.(agentbackend.ModelLister)
			if !ok {
				return fmt.Errorf("backend %q has no live model menu; pass --model with a known id", be.ID())
			}

			models, ok := ml.ListModels()
			if !ok {
				return fmt.Errorf("backend %q could not list its model menu (is %s installed and signed in?)", be.ID(), be.Binary())
			}

			out := cmd.OutOrStdout()
			if asJSON {
				if models == nil {
					models = []string{} // emit [] not null
				}
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(models)
			}
			for _, m := range models {
				fmt.Fprintln(out, m)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&backend, "backend", "", "list models for this backend id (default: the current agent's backend)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the menu as a JSON array instead of one id per line")
	return cmd
}
