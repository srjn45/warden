package cli

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/client"
)

type fakeSource struct {
	scope agentbackend.CatalogScope
	obs   agentbackend.CatalogObservation
	err   error
}

func (f fakeSource) ResolveScope(context.Context) (agentbackend.CatalogScope, error) {
	return f.scope, f.err
}

func (f fakeSource) DiscoverModels(context.Context, agentbackend.CatalogScope) agentbackend.CatalogObservation {
	return f.obs
}

type fakeCatalogAPI struct {
	previews  []backendstore.CatalogRefresh
	applied   []string
	rolled    []uint64
	ops       map[string]int
	applyErr  map[string]error
	previewEr map[string]error
	history   client.CatalogHistory
	backends  []client.Backend
	proposals map[string]backendstore.CatalogProposal
}

func (f *fakeCatalogAPI) PreviewModelCatalog(_ context.Context, r backendstore.CatalogRefresh) (backendstore.CatalogProposal, error) {
	f.previews = append(f.previews, r)
	if err := f.previewEr[r.Scope.AiCli]; err != nil {
		return backendstore.CatalogProposal{}, err
	}
	p := backendstore.CatalogProposal{ID: "prop-" + r.Scope.AiCli, ScopeFP: r.Scope.ScopeFP()}
	if custom, ok := f.proposals[r.Scope.AiCli]; ok {
		custom.ID, custom.ScopeFP = p.ID, p.ScopeFP
		return custom, nil
	}
	for i := 0; i < f.ops[r.Scope.AiCli]; i++ {
		p.Ops = append(p.Ops, backendstore.CatalogProposalOp{Op: "add"})
	}
	return p, nil
}

func (f *fakeCatalogAPI) ApplyModelCatalogProposal(_ context.Context, id string) (backendstore.CatalogRevision, error) {
	f.applied = append(f.applied, id)
	if err := f.applyErr[id]; err != nil {
		return backendstore.CatalogRevision{}, err
	}
	return backendstore.CatalogRevision{Rev: uint64(len(f.applied))}, nil
}

func (f *fakeCatalogAPI) ListBackends(context.Context) (client.BackendsState, error) {
	if f.backends == nil {
		return client.BackendsState{Backends: []client.Backend{
			{ID: "cursor", Installed: true, Enabled: true}, {ID: "codex", Installed: true, Enabled: true},
		}}, nil
	}
	return client.BackendsState{Backends: f.backends}, nil
}

func (f *fakeCatalogAPI) ListModelCatalogHistory(context.Context, string) (client.CatalogHistory, error) {
	return f.history, nil
}

func (f *fakeCatalogAPI) RollbackModelCatalogRevision(_ context.Context, rev uint64) (backendstore.CatalogRevision, error) {
	f.rolled = append(f.rolled, rev)
	return backendstore.CatalogRevision{Rev: rev + 10}, nil
}

func completeObs(ids ...string) agentbackend.CatalogObservation {
	o := agentbackend.CatalogObservation{Source: agentbackend.SourceLiveCommand, Completeness: agentbackend.CatalogComplete}
	for _, id := range ids {
		o.Models = append(o.Models, agentbackend.ObservedModel{ModelID: id})
	}
	return o
}

func stubRefresh(t *testing.T, api *fakeCatalogAPI, srcs map[string]fakeSource) {
	t.Helper()
	origAPI, origSrc := newCatalogRefreshAPI, catalogSourcesFor
	newCatalogRefreshAPI = func(*cobra.Command) catalogRefreshAPI { return api }
	catalogSourcesFor = func(filter []string) ([]catalogSource, []refreshResult, error) {
		var out []catalogSource
		for _, id := range []string{"cursor", "codex"} {
			s, ok := srcs[id]
			if !ok {
				continue
			}
			if len(filter) > 0 && filter[0] != id && (len(filter) < 2 || filter[1] != id) {
				continue
			}
			out = append(out, catalogSource{id: id, src: s})
		}
		return out, nil, nil
	}
	t.Cleanup(func() { newCatalogRefreshAPI, catalogSourcesFor = origAPI, origSrc })
}

func TestModelsRefreshPreviewNeverApplies(t *testing.T) {
	api := &fakeCatalogAPI{ops: map[string]int{"cursor": 2}}
	stubRefresh(t, api, map[string]fakeSource{"cursor": {scope: agentbackend.CatalogScope{AiCli: "cursor", CLIVersion: "1.2"}, obs: completeObs("m-a", "m-b")}})

	out, err := runGit(t, "127.0.0.1:0", "models", "refresh")
	require.NoError(t, err)
	require.Len(t, api.previews, 1)
	require.Equal(t, "1.2", api.previews[0].Scope.CLIVersion)
	require.Len(t, api.previews[0].Observation.Models, 2)
	require.Empty(t, api.applied)
	require.Contains(t, out, "prop-cursor")
	require.Contains(t, out, "preview only")
}

func TestModelsRefreshApplyRequiresYes(t *testing.T) {
	api := &fakeCatalogAPI{}
	stubRefresh(t, api, map[string]fakeSource{"cursor": {obs: completeObs("m")}})

	_, err := runGit(t, "127.0.0.1:0", "models", "refresh", "--apply")
	require.ErrorContains(t, err, "--yes")
	_, err = runGit(t, "127.0.0.1:0", "models", "refresh", "--yes")
	require.ErrorContains(t, err, "--apply")
	require.Empty(t, api.previews, "no discovery or daemon call before confirmation")
}

func TestModelsRefreshApplyAppliesOnlyNonEmptyProposals(t *testing.T) {
	api := &fakeCatalogAPI{ops: map[string]int{"cursor": 1}}
	stubRefresh(t, api, map[string]fakeSource{"cursor": {obs: completeObs("m")}, "codex": {obs: completeObs("c")}})

	out, err := runGit(t, "127.0.0.1:0", "models", "refresh", "--apply", "--yes", "--json")
	require.NoError(t, err)
	require.Equal(t, []string{"prop-cursor"}, api.applied)
	var body struct {
		Applied bool            `json:"applied"`
		Results []refreshResult `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &body))
	require.True(t, body.Applied)
	require.Len(t, body.Results, 2)
	require.True(t, body.Results[1].Applied)
	require.False(t, body.Results[0].Applied)
}

func TestModelsRefreshAicliFilter(t *testing.T) {
	api := &fakeCatalogAPI{}
	stubRefresh(t, api, map[string]fakeSource{"cursor": {obs: completeObs("m")}, "codex": {obs: completeObs("c")}})

	_, err := runGit(t, "127.0.0.1:0", "models", "refresh", "--aicli", "codex")
	require.NoError(t, err)
	require.Len(t, api.previews, 1)
	require.Equal(t, "codex", api.previews[0].Scope.AiCli)
}

func TestModelsRefreshPartialFailureReportsEveryAicli(t *testing.T) {
	api := &fakeCatalogAPI{}
	failed := completeObs()
	failed.Completeness = agentbackend.CatalogFailed
	failed.Err = &agentbackend.CatalogError{Class: "binary_missing"}
	stubRefresh(t, api, map[string]fakeSource{"cursor": {obs: failed}, "codex": {obs: completeObs("c")}})

	out, err := runGit(t, "127.0.0.1:0", "models", "refresh")
	require.ErrorContains(t, err, "partially failed")
	require.Contains(t, out, "discovery failed: binary_missing")
	require.Len(t, api.previews, 1, "a failed discovery is never submitted as evidence")
}

func TestModelsRefreshEveryAicliFailed(t *testing.T) {
	api := &fakeCatalogAPI{}
	stubRefresh(t, api, map[string]fakeSource{"cursor": {err: errors.New("no scope")}})

	_, err := runGit(t, "127.0.0.1:0", "models", "refresh")
	require.ErrorContains(t, err, "failed for every selected aicli")
}

func TestModelsRefreshStaleProposalNotApplied(t *testing.T) {
	api := &fakeCatalogAPI{ops: map[string]int{"cursor": 1}, applyErr: map[string]error{"prop-cursor": &client.StatusError{Code: 409, Msg: "stale"}}}
	stubRefresh(t, api, map[string]fakeSource{"cursor": {obs: completeObs("m")}})

	out, err := runGit(t, "127.0.0.1:0", "models", "refresh", "--apply", "--yes")
	require.Error(t, err)
	require.Contains(t, out, "stale or expired")
}

func TestModelsRefreshForwardsAccountIdentityForFingerprintOnly(t *testing.T) {
	api := &fakeCatalogAPI{}
	stubRefresh(t, api, map[string]fakeSource{"cursor": {scope: agentbackend.CatalogScope{AiCli: "cursor", AccountRef: "acct-1", Profile: "p"}, obs: completeObs("m")}})

	out, err := runGit(t, "127.0.0.1:0", "models", "refresh", "--json")
	require.NoError(t, err)
	got := api.previews[0].Scope
	require.Equal(t, "acct-1", got.AccountRef)
	require.True(t, backendstore.ValidScopeFP(got.ScopeFP()))
	require.NotContains(t, out, "acct-1")
	require.NotContains(t, out, "scope_note")
}

func TestModelsRefreshWithoutAccountIdentityDoesNotClaimAccountScope(t *testing.T) {
	api := &fakeCatalogAPI{}
	stubRefresh(t, api, map[string]fakeSource{"cursor": {scope: agentbackend.CatalogScope{AiCli: "cursor"}, obs: completeObs("m")}})

	out, err := runGit(t, "127.0.0.1:0", "models", "refresh")
	require.NoError(t, err)
	require.Contains(t, out, "not to an account")
}

func TestModelsRollbackUsesDaemonAndRequiresYes(t *testing.T) {
	api := &fakeCatalogAPI{}
	stubRefresh(t, api, nil)

	_, err := runGit(t, "127.0.0.1:0", "models", "rollback", "3")
	require.ErrorContains(t, err, "--yes")
	require.Empty(t, api.rolled)

	_, err = runGit(t, "127.0.0.1:0", "models", "rollback", "3", "--yes")
	require.NoError(t, err)
	require.Equal(t, []uint64{3}, api.rolled)
}

func TestModelsHistoryUsesDaemon(t *testing.T) {
	api := &fakeCatalogAPI{history: client.CatalogHistory{Revisions: []backendstore.CatalogRevision{{Rev: 7, Kind: "apply", ProposalID: "p"}}}}
	stubRefresh(t, api, nil)

	out, err := runGit(t, "127.0.0.1:0", "models", "history")
	require.NoError(t, err)
	require.Contains(t, out, "rev 7")
}

func TestModelsRefreshWithoutIdentityDowngradesCompleteEvidence(t *testing.T) {
	api := &fakeCatalogAPI{}
	stubRefresh(t, api, map[string]fakeSource{"cursor": {scope: agentbackend.CatalogScope{AiCli: "cursor"}, obs: completeObs("m")}})

	_, err := runGit(t, "127.0.0.1:0", "models", "refresh")
	require.NoError(t, err)
	require.Equal(t, backendstore.CompletenessPartial, api.previews[0].Observation.Completeness)
}

func TestModelsRefreshWithIdentityKeepsCompleteEvidence(t *testing.T) {
	api := &fakeCatalogAPI{}
	stubRefresh(t, api, map[string]fakeSource{"cursor": {scope: agentbackend.CatalogScope{AiCli: "cursor", AccountRef: "a"}, obs: completeObs("m")}})

	_, err := runGit(t, "127.0.0.1:0", "models", "refresh")
	require.NoError(t, err)
	require.Equal(t, backendstore.CompletenessComplete, api.previews[0].Observation.Completeness)
}

func TestModelsRefreshDefaultCoversOnlyInstalledEnabled(t *testing.T) {
	api := &fakeCatalogAPI{backends: []client.Backend{
		{ID: "cursor", Installed: true, Enabled: true},
		{ID: "codex", Installed: true, Enabled: false},
	}}
	stubRefresh(t, api, map[string]fakeSource{"cursor": {obs: completeObs("m")}, "codex": {obs: completeObs("c")}})

	_, err := runGit(t, "127.0.0.1:0", "models", "refresh")
	require.NoError(t, err)
	require.Len(t, api.previews, 1)
	require.Equal(t, "cursor", api.previews[0].Scope.AiCli)
}

func TestModelsRefreshExplicitUnavailableAicliReportsStatus(t *testing.T) {
	api := &fakeCatalogAPI{backends: []client.Backend{
		{ID: "cursor", Installed: false, Enabled: true},
		{ID: "codex", Installed: true, Enabled: false},
	}}
	stubRefresh(t, api, map[string]fakeSource{"cursor": {obs: completeObs("m")}, "codex": {obs: completeObs("c")}})

	out, err := runGit(t, "127.0.0.1:0", "models", "refresh", "--aicli", "cursor", "--aicli", "codex")
	require.ErrorContains(t, err, "failed for every selected aicli")
	require.Contains(t, out, "not installed")
	require.Contains(t, out, "disabled in the backend registry")
	require.Empty(t, api.previews)
}

func TestModelsRefreshShowsOperationsAndBlockedReasons(t *testing.T) {
	api := &fakeCatalogAPI{proposals: map[string]backendstore.CatalogProposal{"cursor": {
		Ops: []backendstore.CatalogProposalOp{{
			Op: "retire", Key: "cursor:old-model", Reasons: []string{"absent from complete source"},
			Before: &backendstore.ModelEntry{Tier: backendstore.Tier1, AutoAssign: true},
			After:  &backendstore.ModelEntry{Tier: backendstore.Tier1, Lifecycle: "retired"},
		}},
		Blocked: []backendstore.CatalogBlockedOp{{Key: "cursor:x", Op: "retire", Reason: "mass-retire guard"}},
	}}}
	stubRefresh(t, api, map[string]fakeSource{"cursor": {scope: agentbackend.CatalogScope{AiCli: "cursor", AccountRef: "a"}, obs: completeObs("m")}})

	out, err := runGit(t, "127.0.0.1:0", "models", "refresh")
	require.NoError(t, err)
	require.Contains(t, out, "cursor retire cursor:old-model -> tier-1/retired/auto=false (was tier-1/active/auto=true): absent from complete source")
	require.Contains(t, out, "blocked retire cursor:x: mass-retire guard")

	jsonOut, err := runGit(t, "127.0.0.1:0", "models", "refresh", "--json")
	require.NoError(t, err)
	var body struct {
		Results []refreshResult `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &body))
	require.Len(t, body.Results[0].Operations, 1)
	require.Equal(t, "retired", body.Results[0].Operations[0].After.Lifecycle)
	require.Len(t, body.Results[0].BlockedOps, 1)
	require.Equal(t, "live_command", body.Results[0].Source)
	require.NotEmpty(t, body.Results[0].ScopeFP)
}

func TestInstalledCatalogSourcesHonoursFilterForEveryRegisteredAiCli(t *testing.T) {
	all, statuses, err := installedCatalogSources(nil)
	require.NoError(t, err)
	require.Empty(t, statuses)
	require.GreaterOrEqual(t, len(all), 2)

	for _, want := range all {
		got, statuses, err := installedCatalogSources([]string{want.id})
		require.NoError(t, err)
		require.Empty(t, statuses)
		require.Len(t, got, 1, "filter %q must not leak other registered AI CLIs", want.id)
		require.Equal(t, want.id, got[0].id)
	}

	pair, _, err := installedCatalogSources([]string{all[0].id, all[len(all)-1].id})
	require.NoError(t, err)
	require.Len(t, pair, 2)
}

func TestInstalledCatalogSourcesUnsupportedAndUnknownAiCli(t *testing.T) {
	got, statuses, err := installedCatalogSources([]string{"claude"})
	require.NoError(t, err)
	require.Empty(t, got)
	require.Len(t, statuses, 1)
	require.Equal(t, "claude", statuses[0].AiCli)
	require.Contains(t, statuses[0].Error, "no model discovery source")

	_, _, err = installedCatalogSources([]string{"no-such-aicli"})
	require.ErrorContains(t, err, "unknown aicli")
}
