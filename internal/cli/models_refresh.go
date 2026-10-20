package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/client"
)

// catalogRefreshAPI is the daemon surface `models refresh` uses. The daemon is the
// only writer of catalog policy; the CLI never opens the registry for this.
type catalogRefreshAPI interface {
	PreviewModelCatalog(ctx context.Context, refresh backendstore.CatalogRefresh) (backendstore.CatalogProposal, error)
	ApplyModelCatalogProposal(ctx context.Context, proposalID string) (backendstore.CatalogRevision, error)
	ListBackends(ctx context.Context) (client.BackendsState, error)
	ListModelCatalogHistory(ctx context.Context, aicli string) (client.CatalogHistory, error)
	RollbackModelCatalogRevision(ctx context.Context, revision uint64) (backendstore.CatalogRevision, error)
}

// Seams so tests drive refresh with a fake daemon and fake discovery sources.
var (
	newCatalogRefreshAPI = func(cmd *cobra.Command) catalogRefreshAPI { return clientFor(cmd) }
	catalogSourcesFor    = installedCatalogSources
	refreshSourceTimeout = 30 * time.Second
)

type catalogSource struct {
	id  string
	src agentbackend.ModelCatalogSource
}

// installedCatalogSources returns every registered AI CLI that can discover its
// own models, restricted to filter when non-empty. An explicitly requested AI CLI
// without a discovery source yields a status result; an unknown id is an error.
func installedCatalogSources(filter []string) ([]catalogSource, []refreshResult, error) {
	want := map[string]bool{}
	for _, f := range filter {
		if f = strings.TrimSpace(f); f != "" {
			want[f] = true
		}
	}
	filtering := len(want) > 0
	var out []catalogSource
	var statuses []refreshResult
	ids := agentbackend.IDs()
	sort.Strings(ids)
	for _, id := range ids {
		if filtering && !want[id] {
			continue
		}
		delete(want, id)
		b, err := agentbackend.Get(id)
		if err != nil {
			continue
		}
		if s, ok := b.(agentbackend.ModelCatalogSource); ok {
			out = append(out, catalogSource{id: id, src: s})
		} else if filtering {
			statuses = append(statuses, refreshResult{AiCli: id, Error: "no model discovery source (manual catalog only: use `warden models add/tier`)"})
		}
	}
	for id := range want {
		return nil, nil, fmt.Errorf("unknown aicli %q (known: %s)", id, strings.Join(agentbackend.IDs(), ", "))
	}
	return out, statuses, nil
}

// refreshResult is one AI CLI's outcome; Error is set for a partial failure.
type refreshResult struct {
	AiCli           string        `json:"aicli"`
	Completeness    string        `json:"completeness,omitempty"`
	Source          string        `json:"source,omitempty"`
	SourceFetchedAt time.Time     `json:"source_fetched_at,omitzero"`
	ScopeFP         string        `json:"scope_fp,omitempty"`
	ProposalID      string        `json:"proposal_id,omitempty"`
	Ops             int           `json:"ops"`
	Blocked         int           `json:"blocked"`
	Operations      []opView      `json:"operations,omitempty"`
	BlockedOps      []blockedView `json:"blocked_operations,omitempty"`
	Applied         bool          `json:"applied"`
	Revision        uint64        `json:"revision,omitempty"`
	Error           string        `json:"error,omitempty"`
	ScopeNote       string        `json:"scope_note,omitempty"`
}

type opView struct {
	Op      string   `json:"op"`
	Key     string   `json:"key"`
	Reasons []string `json:"reasons,omitempty"`
	Before  *rowView `json:"before,omitempty"`
	After   *rowView `json:"after,omitempty"`
}

type rowView struct {
	Tier       string `json:"tier,omitempty"`
	Lifecycle  string `json:"lifecycle,omitempty"`
	AutoAssign bool   `json:"auto_assign"`
}

type blockedView struct {
	Key    string `json:"key"`
	Op     string `json:"op"`
	Reason string `json:"reason"`
}

func rowOf(m *backendstore.ModelEntry) *rowView {
	if m == nil {
		return nil
	}
	return &rowView{Tier: string(m.Tier), Lifecycle: m.Lifecycle, AutoAssign: m.AutoAssign}
}

func describeRow(r *rowView) string {
	if r == nil {
		return "-"
	}
	lc := r.Lifecycle
	if lc == "" {
		lc = "active"
	}
	return fmt.Sprintf("%s/%s/auto=%t", r.Tier, lc, r.AutoAssign)
}

func toStoreObservation(o agentbackend.CatalogObservation) backendstore.CatalogObservation {
	out := backendstore.CatalogObservation{
		Source: backendstore.SourceKind(o.Source), SourceDetail: o.SourceDetail,
		SourceFetchedAt: o.SourceFetchedAt, Completeness: backendstore.Completeness(o.Completeness),
	}
	if o.Err != nil {
		out.Err = &backendstore.CatalogError{Code: o.Err.Class, Message: o.Err.Detail}
	}
	for _, m := range o.Models {
		out.Models = append(out.Models, backendstore.ObservedModel{
			ModelID: m.ModelID, DisplayName: m.DisplayName, Visibility: m.Visibility,
			ReasoningLevels: m.ReasoningLevels, DefaultReasoning: m.DefaultReasoning,
			ContextWindow: m.ContextWindow, UpgradeTo: m.UpgradeTo,
		})
	}
	return out
}

func newModelsRefreshCmd() *cobra.Command {
	var aiclis []string
	var apply, yes, asJSON bool
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "Preview (and with --apply --yes, apply) a model-catalog refresh via the daemon",
		Long: `Discover the models each installed AI CLI reports about itself (local files and
non-generating metadata commands only: no prompt, no login, no token read),
submit that evidence to the daemon as a catalog proposal, and print the diff.

Without --apply this is a preview: it records evidence and a proposal but never
changes routing policy. With --apply --yes it applies exactly the proposals it
just created (blocked operations are reported, never applied). A stale or expired
proposal is refused: re-run. Partial, empty, failed or offline discovery never
retires a model.

Applying changes catalog policy only. Pinned models (explicit --aicli + --model),
running, restored, forked, scheduled and pipeline sessions are untouched. To move
an existing agent onto the refreshed catalog, run an explicit hot-swap afterwards:
  warden switch AGENT --aicli ID --tier tier-N     (catalog pick within that AI CLI)
  warden switch AGENT --tier tier-N                (router pick across AI CLIs)
  warden switch AGENT --aicli ID --model MODEL     (explicit pin)

Examples:
  warden models refresh --aicli cursor
  warden models refresh --aicli cursor --aicli codex --json
  warden models refresh --apply --yes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if apply && !yes {
				return fmt.Errorf("--apply changes catalog policy; re-run with --apply --yes after reviewing the preview (it does not switch live sessions)")
			}
			if yes && !apply {
				return fmt.Errorf("--yes only confirms --apply")
			}
			sources, results, err := catalogSourcesFor(aiclis)
			if err != nil {
				return err
			}
			api := newCatalogRefreshAPI(cmd)
			state, err := api.ListBackends(cmd.Context())
			if err != nil {
				return fmt.Errorf("read the backend registry from the daemon: %w", err)
			}
			registry := map[string]client.Backend{}
			for _, b := range state.Backends {
				registry[b.ID] = b
			}
			var selected []catalogSource
			for _, cs := range sources {
				reg, known := registry[cs.id]
				switch {
				case len(aiclis) == 0 && (!known || !reg.Installed || !reg.Enabled):
					continue
				case !known || !reg.Installed:
					results = append(results, refreshResult{AiCli: cs.id, Error: "not installed"})
				case !reg.Enabled:
					results = append(results, refreshResult{AiCli: cs.id, Error: "disabled in the backend registry"})
				default:
					selected = append(selected, cs)
				}
			}
			if len(selected) == 0 && len(results) == 0 {
				return fmt.Errorf("no installed, enabled AI CLI with a model discovery source")
			}
			for _, cs := range selected {
				results = append(results, refreshOne(cmd.Context(), api, cs, apply))
			}
			sort.Slice(results, func(i, j int) bool { return results[i].AiCli < results[j].AiCli })

			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(map[string]any{"applied": apply, "results": results}); err != nil {
					return err
				}
			} else {
				w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
				fmt.Fprintln(w, "AICLI\tEVIDENCE\tPROPOSAL\tOPS\tBLOCKED\tAPPLIED\tNOTE")
				for _, r := range results {
					applied := "no"
					if r.Applied {
						applied = fmt.Sprintf("rev %d", r.Revision)
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%s\t%s\n", r.AiCli, r.Completeness, r.ProposalID, r.Ops, r.Blocked, applied, r.Error)
				}
				if err := w.Flush(); err != nil {
					return err
				}
				for _, r := range results {
					for _, o := range r.Operations {
						fmt.Fprintf(out, "  %s %s %s -> %s", r.AiCli, o.Op, o.Key, describeRow(o.After))
						if o.Before != nil {
							fmt.Fprintf(out, " (was %s)", describeRow(o.Before))
						}
						if len(o.Reasons) > 0 {
							fmt.Fprintf(out, ": %s", strings.Join(o.Reasons, "; "))
						}
						fmt.Fprintln(out)
					}
					for _, bl := range r.BlockedOps {
						fmt.Fprintf(out, "  %s blocked %s %s: %s\n", r.AiCli, bl.Op, bl.Key, bl.Reason)
					}
					if r.ScopeNote != "" {
						fmt.Fprintf(out, "note (%s): %s\n", r.AiCli, r.ScopeNote)
					}
				}
				if !apply {
					fmt.Fprintln(out, "preview only: routing unchanged. Re-run with --apply --yes to apply.")
				}
			}
			failed := 0
			for _, r := range results {
				if r.Error != "" {
					failed++
				}
			}
			switch {
			case failed == len(results):
				return fmt.Errorf("model refresh failed for every selected aicli")
			case failed > 0:
				return fmt.Errorf("model refresh partially failed (%d of %d aicli); see the per-aicli notes above", failed, len(results))
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&aiclis, "aicli", nil, "limit to this AI CLI (repeatable; default: every installed AI CLI that can discover models)")
	cmd.Flags().BoolVar(&apply, "apply", false, "apply the proposals this command creates (requires --yes)")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm --apply")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit structured per-aicli results")
	return cmd
}

func refreshOne(ctx context.Context, api catalogRefreshAPI, cs catalogSource, apply bool) refreshResult {
	res := refreshResult{AiCli: cs.id}
	dctx, cancel := context.WithTimeout(ctx, refreshSourceTimeout)
	defer cancel()
	scope, err := cs.src.ResolveScope(dctx)
	if err != nil {
		res.Error = "scope: " + err.Error()
		return res
	}
	if scope.AiCli == "" {
		scope.AiCli = cs.id
	}
	obs := cs.src.DiscoverModels(dctx, scope)
	if scope.AccountRef == "" && scope.Profile == "" {
		res.ScopeNote = "the AI CLI reported no account or profile identity: this evidence is scoped to the AI CLI and auth mode only, not to an account, so it is submitted as partial and can never retire a model"
		if obs.Completeness == agentbackend.CatalogComplete {
			obs.Completeness = agentbackend.CatalogPartial
		}
	}
	res.Completeness = string(obs.Completeness)
	res.Source, res.SourceFetchedAt = string(obs.Source), obs.SourceFetchedAt
	if obs.Completeness == agentbackend.CatalogFailed {
		res.Error = "discovery failed"
		if obs.Err != nil {
			res.Error += ": " + obs.Err.Class
		}
		return res
	}
	p, err := api.PreviewModelCatalog(ctx, backendstore.CatalogRefresh{
		Scope: backendstore.CatalogScope{
			AiCli: scope.AiCli, AuthMode: scope.AuthMode, AccountRef: scope.AccountRef, Profile: scope.Profile, PlanLabel: scope.PlanLabel, CLIVersion: scope.CLIVersion,
		},
		Observation: toStoreObservation(obs),
		ObservedAt:  time.Now().UTC(),
	})
	if err != nil {
		res.Error = "preview: " + err.Error()
		return res
	}
	res.ProposalID, res.ScopeFP, res.Ops, res.Blocked = p.ID, p.ScopeFP, len(p.Ops), len(p.Blocked)
	for _, o := range p.Ops {
		res.Operations = append(res.Operations, opView{Op: o.Op, Key: o.Key, Reasons: o.Reasons, Before: rowOf(o.Before), After: rowOf(o.After)})
	}
	for _, bl := range p.Blocked {
		res.BlockedOps = append(res.BlockedOps, blockedView{Key: bl.Key, Op: bl.Op, Reason: bl.Reason})
	}
	if !apply || len(p.Ops) == 0 {
		return res
	}
	rev, err := api.ApplyModelCatalogProposal(ctx, p.ID)
	if err != nil {
		var se *client.StatusError
		if errors.As(err, &se) && se.Code == 409 {
			res.Error = "proposal stale or expired; re-run refresh"
		} else {
			res.Error = "apply: " + err.Error()
		}
		return res
	}
	res.Applied, res.Revision = true, rev.Rev
	return res
}
