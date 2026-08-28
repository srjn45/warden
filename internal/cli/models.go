package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/agentbackend"
	_ "github.com/srjn45/warden/internal/agentbackend/backends" // register the adapters so agentbackend.Get resolves in-process
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/config"
)

// openBackendStore opens the backend registry store for CLI commands.
// It is a package var so tests can stub it with a test store.
var openBackendStore = func(cmd *cobra.Command) (*backendstore.Store, error) {
	cfg := config.Load(configPathFor(cmd))
	return backendstore.NewStore(filepath.Join(cfg.DataDir, "backends"))
}

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
		Short: "Inspect and manage model catalog and live model menus",
		Long: "Show the live, currently-available model menu the agent's backend exposes, or\n" +
			"inspect and configure the tiered model catalog in warden.\n\n" +
			"Subcommands:\n" +
			"  list      List models in the catalog and their assigned tiers\n" +
			"  tier      Set a model's tier classification (tier-1|tier-2|tier-3)\n\n" +
			"When run without subcommands, `warden models` shows the live model menu of the\n" +
			"current or specified backend.",
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

	cmd.AddCommand(newModelsListCmd(), newModelsTierCmd())
	return cmd
}

func newModelsListCmd() *cobra.Command {
	var byTier bool
	var tier string
	var backend string
	var asJSON bool

	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List models in the catalog and their assigned tiers",
		Aliases: []string{"ls"},
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openBackendStore(cmd)
			if err != nil {
				return err
			}
			defer st.Close()

			var tierFilter backendstore.ModelTier
			if tier != "" {
				tierFilter = backendstore.ModelTier(tier)
				if !tierFilter.Valid() {
					return fmt.Errorf("invalid tier %q (valid: tier-1, tier-2, tier-3)", tier)
				}
			}

			models, err := st.ListModels(tierFilter)
			if err != nil {
				return err
			}

			if backend != "" {
				var filtered []backendstore.ModelEntry
				for _, m := range models {
					if m.BackendID == backend {
						filtered = append(filtered, m)
					}
				}
				models = filtered
			}

			out := cmd.OutOrStdout()
			if asJSON {
				if models == nil {
					models = []backendstore.ModelEntry{}
				}
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(models)
			}

			if byTier {
				return printModelsByTier(out, models)
			}
			return printModelsTable(out, models)
		},
	}

	cmd.Flags().BoolVar(&byTier, "by-tier", false, "group and display models by tier")
	cmd.Flags().StringVar(&tier, "tier", "", "filter models by tier (tier-1|tier-2|tier-3)")
	cmd.Flags().StringVar(&backend, "backend", "", "filter models by backend ID")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit models as a JSON array")
	return cmd
}

func newModelsTierCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tier <backend> <model> <tier>",
		Short: "Set a model's tier classification (tier-1|tier-2|tier-3)",
		Long: `Set a model's tier classification in the catalog.

Tiers:
  tier-1   Highest-capability models (e.g. Claude Opus, o1) for architecture, design, and complex planning
  tier-2   Standard implementation models (e.g. Claude Sonnet, Gemini Pro, GPT-4.1) for everyday coding
  tier-3   Fast, low-cost models (e.g. Claude Haiku, Gemini Flash, GPT-4.1-mini) for quick tasks and CI triage

Example:
  warden models tier claude sonnet tier-2`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			backendID := args[0]
			modelID := args[1]
			tierStr := args[2]

			tier := backendstore.ModelTier(tierStr)
			if !tier.Valid() {
				return fmt.Errorf("invalid tier %q (valid: tier-1, tier-2, tier-3)", tierStr)
			}

			st, err := openBackendStore(cmd)
			if err != nil {
				return err
			}
			defer st.Close()

			if err := st.SetModelTier(backendID, modelID, tier); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "model %s/%s tiered as %s\n", backendID, modelID, tier)
			return nil
		},
	}
}

func printModelsTable(out io.Writer, models []backendstore.ModelEntry) error {
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "BACKEND\tMODEL\tTIER\tDISPLAY NAME\tENABLED\tDEFAULT")
	for _, m := range models {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			m.BackendID,
			m.ModelID,
			m.Tier,
			m.DisplayName,
			checkMark(m.Enabled),
			checkMark(m.IsDefault),
		)
	}
	return w.Flush()
}

func printModelsByTier(out io.Writer, models []backendstore.ModelEntry) error {
	tiers := []backendstore.ModelTier{backendstore.Tier1, backendstore.Tier2, backendstore.Tier3}
	tierMap := make(map[backendstore.ModelTier][]backendstore.ModelEntry)
	for _, m := range models {
		tierMap[m.Tier] = append(tierMap[m.Tier], m)
	}

	first := true
	for _, t := range tiers {
		entries := tierMap[t]
		if len(entries) == 0 {
			continue
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].BackendID == entries[j].BackendID {
				return entries[i].ModelID < entries[j].ModelID
			}
			return entries[i].BackendID < entries[j].BackendID
		})

		if !first {
			fmt.Fprintln(out)
		}
		first = false

		fmt.Fprintf(out, "=== %s ===\n", strings.ToUpper(string(t)))
		w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "BACKEND\tMODEL\tDISPLAY NAME\tENABLED\tDEFAULT")
		for _, m := range entries {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				m.BackendID,
				m.ModelID,
				m.DisplayName,
				checkMark(m.Enabled),
				checkMark(m.IsDefault),
			)
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
	return nil
}
