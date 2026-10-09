package backendstore

// Reusable fixtures and preservation assertions for issue #841; see
// docs/specs/2026-10-09-backend-registry-integrity-contract.md. Everything here
// builds its damaged store programmatically inside a caller-supplied (t.TempDir)
// directory; nothing reads or mutates a real ~/.warden.

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/srjn45/scriva/store"
	"github.com/stretchr/testify/require"
)

// Fixed identities used by buildRegistry; the preservation helpers key off them.
const (
	fxCustomBackend = "fx-custom"
	fxCustomModel   = "fx-model"
	fxCooldownModel = "fx-cool"
	fxQuotaBackend  = "claude"
)

// registrySnapshot is the user-owned state a recovery must not lose.
type registrySnapshot struct {
	Backends   []Backend
	Settings   Settings
	Models     []ModelEntry
	RoleTiers  []RoleTierMapping
	Handover   HandoverSettings
	Quotas     []BackendQuota
	Default    string
	CoolingFor []string // "backend/model" pairs that report an active cooldown
}

// buildRegistry creates a store in dir holding deliberate user overrides in
// every collection, closes it, and returns the snapshot it held. Every backend
// row is updated several times so its segment history has revisions > 1.
func buildRegistry(t *testing.T, dir string) registrySnapshot {
	t.Helper()
	s, err := NewStore(dir)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, s.Upsert(Backend{ID: "claude", Installed: true, BinaryPath: "/usr/bin/claude", DetectedAt: now, Tier: TierUnclassified, Enabled: true}))
	require.NoError(t, s.Upsert(Backend{ID: fxCustomBackend, Installed: true, DetectedAt: now, Tier: TierUnclassified, Enabled: true}))
	// Several user edits per row: these build the revision history that a
	// second writer later regresses.
	for _, tier := range []string{TierFree, TierSubscription, TierPayPerUse, TierSubscription} {
		require.NoError(t, s.SetTier("claude", tier))
	}
	for _, tier := range []string{TierPayPerUse, TierFree, TierFree} {
		require.NoError(t, s.SetTier(fxCustomBackend, tier))
	}
	require.NoError(t, s.SetEnabled(fxCustomBackend, false))
	require.NoError(t, s.SetEnabled(fxCustomBackend, true))
	require.NoError(t, s.SetEnabled(fxCustomBackend, false))
	require.NoError(t, s.SetDefault("claude"))

	require.NoError(t, s.SetThinkingMode(ThinkingModeLocalOnly))
	require.NoError(t, s.SetAllowPaidAutopilot(true))

	require.NoError(t, s.AddModel(fxCustomBackend, fxCustomModel, "Fixture", Tier3, false, ""))
	require.NoError(t, s.SetModelTier(fxCustomBackend, fxCustomModel, Tier2))
	require.NoError(t, s.SetModelTier(fxCustomBackend, fxCustomModel, Tier1))
	require.NoError(t, s.SetModelEnabled(fxCustomBackend, fxCustomModel, false))

	require.NoError(t, s.SetRoleTier("implementer", Tier3))
	require.NoError(t, s.SetRoleTier("implementer", Tier2))
	require.NoError(t, s.SetRoleTier("implementer", Tier1))

	require.NoError(t, s.SetHandoverSettings(HandoverSettings{Enabled: false, ContextFillThreshold: 77, CooldownPeriod: 42 * time.Minute}))

	require.NoError(t, s.SetQuotaLimit(fxQuotaBackend, 1234, QuotaWindowType("rolling"), 5*time.Hour))
	require.NoError(t, s.SetQuotaLimit(fxQuotaBackend, 4321, QuotaWindowType("rolling"), 6*time.Hour))
	require.NoError(t, s.SetRLCooldown(fxQuotaBackend, fxCooldownModel, now.Add(24*time.Hour)))

	snap := snapshotRegistry(t, s)
	require.NoError(t, s.Close())
	return snap
}

// snapshotRegistry reads every user-owned collection through the public API.
func snapshotRegistry(t *testing.T, s *Store) registrySnapshot {
	t.Helper()
	var snap registrySnapshot
	var err error
	snap.Backends, err = s.List()
	require.NoError(t, err)
	snap.Settings, err = s.Settings()
	require.NoError(t, err)
	snap.Models, err = s.ListModels("")
	require.NoError(t, err)
	snap.RoleTiers, err = s.ListRoleTiers()
	require.NoError(t, err)
	snap.Handover, err = s.GetHandoverSettings()
	require.NoError(t, err)
	snap.Quotas, err = s.ListQuotas()
	require.NoError(t, err)
	if d, ok, derr := s.Default(); derr == nil && ok {
		snap.Default = d.ID
	}
	if s.IsRLCoolingDown(fxQuotaBackend, fxCooldownModel, time.Time{}) {
		snap.CoolingFor = append(snap.CoolingFor, fxQuotaBackend+"/"+fxCooldownModel)
	}
	return snap
}

// requireRegistryPreserved is the t2 acceptance assertion: got must carry every
// user-owned fact in want. Volatile fields (quota UpdatedAt) are ignored.
func requireRegistryPreserved(t *testing.T, want, got registrySnapshot) {
	t.Helper()
	require.Equal(t, want.Backends, got.Backends, "backend rows (tier, enabled, default, detection)")
	require.Equal(t, want.Default, got.Default, "default backend")
	require.Equal(t, want.Settings, got.Settings, "settings (thinking mode, allow-paid)")
	require.Equal(t, want.Models, got.Models, "model catalog (tiers, enabled, custom)")
	require.Equal(t, want.RoleTiers, got.RoleTiers, "role tier mappings")
	require.Equal(t, want.Handover, got.Handover, "handover settings")
	require.Equal(t, want.CoolingFor, got.CoolingFor, "rate-limit cooldowns")
	require.Len(t, got.Quotas, len(want.Quotas), "quota rows")
	for i := range want.Quotas {
		w, g := want.Quotas[i], got.Quotas[i]
		w.UpdatedAt, g.UpdatedAt = time.Time{}, time.Time{}
		require.Equal(t, w, g, "quota %s/%s", w.BackendID, w.Scope)
	}
}

// injectRevisionRegressions reproduces the v9.25-era #841 finding in dir/col: for
// every id whose history has an update at a revision below the id's latest, the
// older (CRC-valid) update line is re-appended to the newest segment, exactly
// what a second writer replaying a stale handle produces. It also removes the
// derived index files so the next open must scan the segments (and so runs the
// integrity gate). It returns the number of regressions injected.
func injectRevisionRegressions(t *testing.T, dir, col string) int {
	t.Helper()
	cdir := filepath.Join(dir, col)
	segs, err := filepath.Glob(filepath.Join(cdir, "seg_*.ndjson"))
	require.NoError(t, err)
	require.NotEmpty(t, segs)
	last := segs[len(segs)-1]
	raw, err := os.ReadFile(last)
	require.NoError(t, err)

	type line struct {
		ID  uint64 `json:"id"`
		Op  string `json:"op"`
		Rev uint64 `json:"rev"`
	}
	maxRev := map[uint64]uint64{}
	older := map[uint64][]byte{}
	for _, l := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var e line
		require.NoError(t, json.Unmarshal(l, &e))
		if e.Rev > maxRev[e.ID] {
			maxRev[e.ID] = e.Rev
		}
		if e.Op == "update" {
			if _, ok := older[e.ID]; !ok {
				older[e.ID] = append([]byte(nil), l...) // first update = lowest update rev
			}
		}
	}
	var extra []byte
	n := 0
	for id, l := range older {
		var e line
		require.NoError(t, json.Unmarshal(l, &e))
		if e.Rev < maxRev[id] {
			extra = append(extra, l...)
			extra = append(extra, '\n')
			n++
		}
	}
	f, err := os.OpenFile(last, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.Write(extra)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	idx, _ := filepath.Glob(filepath.Join(cdir, "index.json"))
	sidx, _ := filepath.Glob(filepath.Join(cdir, "sidx_*.json"))
	for _, p := range append(idx, sidx...) {
		require.NoError(t, os.Remove(p))
	}
	return n
}

// damagedRegistry builds a registry, regresses revisions in the given collections
// and returns dir plus the pre-damage snapshot.
func damagedRegistry(t *testing.T, cols ...string) (string, registrySnapshot) {
	t.Helper()
	dir := t.TempDir()
	want := buildRegistry(t, dir)
	for _, c := range cols {
		require.Positive(t, injectRevisionRegressions(t, dir, c), "fixture must create regressions in %s", c)
	}
	return dir, want
}

// injectDivergentWrites reproduces the exact #841 shape: a second writer appends
// n revision-68 updates PER ID with a NEWER timestamp after revision 70.
// mutate edits the copied winner data of each injected line (index j).
// Derived index files are removed so the next open scans. It returns the number
// of lines injected.
func injectDivergentWrites(t *testing.T, dir, col string, n int, mutate func(j int, id uint64, data map[string]any)) int {
	t.Helper()
	cdir := filepath.Join(dir, col)
	segs, err := filepath.Glob(filepath.Join(cdir, "seg_*.ndjson"))
	require.NoError(t, err)
	last := segs[len(segs)-1]
	raw, err := os.ReadFile(last)
	require.NoError(t, err)
	type win struct {
		rev  uint64
		data map[string]any
	}
	wins := map[uint64]win{}
	for _, l := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var e store.Entry
		require.NoError(t, json.Unmarshal(l, &e))
		if e.Rev >= wins[e.ID].rev {
			wins[e.ID] = win{e.Rev, e.Data}
		}
	}
	// The settings singleton shares the backends collection but was not part of
	// the incident. Keep this fixture to the two actual backend rows: 17 each.
	for id, w := range wins {
		if key, _ := w.data["_key"].(string); key == "__settings__" {
			delete(wins, id)
		}
	}
	// Raise the original winning line to revision 70. The fixture's existing
	// history remains monotone, then the stale writer appends rev 68 afterward.
	var rewritten []byte
	for _, l := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var e store.Entry
		require.NoError(t, json.Unmarshal(l, &e))
		if w, ok := wins[e.ID]; ok && e.Rev == w.rev {
			e.Rev = 70
			b, err := store.Encode(e)
			require.NoError(t, err)
			rewritten = append(rewritten, b...)
			continue
		}
		rewritten = append(rewritten, l...)
		rewritten = append(rewritten, '\n')
	}
	require.NoError(t, os.WriteFile(last, rewritten, 0o600))
	var extra []byte
	count := 0
	ids := make([]uint64, 0, len(wins))
	for id := range wins {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		w := wins[id]
		for j := 0; j < n; j++ {
			data := map[string]any{}
			maps.Copy(data, w.data)
			mutate(j, id, data)
			b, err := store.Encode(store.Entry{ID: id, Op: store.OpUpdate, Ts: time.Now().UTC().Add(time.Hour + time.Duration(j)*time.Second), Rev: 68, Data: data})
			require.NoError(t, err)
			extra = append(extra, b...)
			count++
		}
	}
	f, err := os.OpenFile(last, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.Write(extra)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	idx, _ := filepath.Glob(filepath.Join(cdir, "index.json"))
	sidx, _ := filepath.Glob(filepath.Join(cdir, "sidx_*.json"))
	for _, p := range append(idx, sidx...) {
		require.NoError(t, os.Remove(p))
	}
	return count
}

// divergentRegistry builds a registry then applies injectDivergentWrites to "backends".
func divergentRegistry(t *testing.T, mutate func(j int, id uint64, data map[string]any)) (string, registrySnapshot, int) {
	t.Helper()
	dir := t.TempDir()
	want := buildRegistry(t, dir)
	n := injectDivergentWrites(t, dir, "backends", 17, mutate)
	return dir, want, n
}
