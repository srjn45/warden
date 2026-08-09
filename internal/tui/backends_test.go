package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/client"
)

// backendsFixture is a small registry with the reserved local row, sorted
// id-ascending exactly as the daemon (and sortBackendsState) return it:
//
//	0: claude (free, default, enabled)
//	1: codex  (subscription, disabled)
//	2: local  (local tier, IsLocal, enabled)
func backendsFixture() client.BackendsState {
	return client.BackendsState{
		Backends: []client.Backend{
			{ID: "claude", Installed: true, Tier: backendstore.TierFree, Default: true, Enabled: true},
			{ID: "codex", Installed: true, Tier: backendstore.TierSubscription, Enabled: false},
			{ID: "local", Installed: true, Tier: backendstore.TierLocal, IsLocal: true, Enabled: true},
		},
		Settings: client.BackendSettings{InternalThinkingMode: backendstore.ThinkingModeFreePlusLocal},
	}
}

// backendsModel returns a control pane sitting on the Backends page with the fixture
// loaded and the cursor on the given row.
func backendsModel(cursor int) controlPaneModel {
	m := newListPane(&fakeAPI{backends: backendsFixture()}, "", "")
	m.ready = true
	m.mode = modeBackends
	m.backendsState = backendsFixture()
	m.backendCursor = cursor
	return m
}

func TestBackendsOpenFromNav(t *testing.T) {
	m := newListPane(&fakeAPI{}, "", "")
	m.ready = true
	updated, cmd := m.handleKey(key("b"))
	um := updated.(controlPaneModel)
	if um.mode != modeBackends {
		t.Fatalf("b should open modeBackends, got %v", um.mode)
	}
	if cmd == nil {
		t.Fatalf("opening the Backends page should kick off a load cmd")
	}
	// The load cmd fetches the registry.
	msg := cmd()
	if _, ok := msg.(backendsMsg); !ok {
		t.Fatalf("load cmd should produce a backendsMsg, got %T", msg)
	}
}

func TestBackendsMsgLoadsAndSorts(t *testing.T) {
	m := backendsModel(0)
	// Feed an unsorted state; the handler must sort it id-ascending.
	unsorted := client.BackendsState{Backends: []client.Backend{
		{ID: "local", IsLocal: true},
		{ID: "claude"},
	}}
	updated, _ := m.Update(backendsMsg{state: unsorted})
	um := updated.(controlPaneModel)
	if got := um.backendsState.Backends[0].ID; got != "claude" {
		t.Fatalf("rows should be sorted id-ascending, first = %q", got)
	}
}

func TestBackendsNavigation(t *testing.T) {
	m := backendsModel(0)
	updated, _ := m.handleKey(key("j"))
	if got := updated.(controlPaneModel).backendCursor; got != 1 {
		t.Fatalf("j should move cursor to 1, got %d", got)
	}
	// Cannot move past the last row.
	m = backendsModel(2)
	updated, _ = m.handleKey(key("j"))
	if got := updated.(controlPaneModel).backendCursor; got != 2 {
		t.Fatalf("j on the last row should stay at 2, got %d", got)
	}
	// Cannot move above the first row.
	m = backendsModel(0)
	updated, _ = m.handleKey(key("k"))
	if got := updated.(controlPaneModel).backendCursor; got != 0 {
		t.Fatalf("k on the first row should stay at 0, got %d", got)
	}
}

func TestBackendsCycleTier(t *testing.T) {
	m := backendsModel(0) // claude, currently free
	_, cmd := m.handleKey(key("t"))
	if cmd == nil {
		t.Fatalf("t should return a set-tier cmd")
	}
	cmd()
	fa := m.api.(*fakeAPI)
	if fa.tieredID != "claude" {
		t.Fatalf("want tieredID=claude, got %q", fa.tieredID)
	}
	if fa.tieredTier != backendstore.TierSubscription {
		t.Fatalf("free should cycle to subscription, got %q", fa.tieredTier)
	}
}

func TestBackendsCycleTierRejectedOnLocal(t *testing.T) {
	m := backendsModel(2) // the local row
	_, cmd := m.handleKey(key("t"))
	if cmd != nil {
		t.Fatalf("t on the local row must be a no-op (tier is system-set)")
	}
}

func TestBackendsSetDefault(t *testing.T) {
	// Both d and enter set the row under the cursor as the default.
	for _, k := range []string{"d", "enter"} {
		m := backendsModel(1) // codex
		_, cmd := m.handleKey(key(k))
		if cmd == nil {
			t.Fatalf("%q should return a set-default cmd", k)
		}
		cmd()
		if got := m.api.(*fakeAPI).defaultedID; got != "codex" {
			t.Fatalf("%q: want defaultedID=codex, got %q", k, got)
		}
	}
}

func TestBackendsSetDefaultRejectedOnLocal(t *testing.T) {
	m := backendsModel(2) // the local row
	_, cmd := m.handleKey(key("d"))
	if cmd != nil {
		t.Fatalf("d on the local row must be a no-op (local cannot be the default)")
	}
}

func TestBackendsToggleEnabled(t *testing.T) {
	m := backendsModel(1) // codex, currently disabled
	_, cmd := m.handleKey(key("e"))
	if cmd == nil {
		t.Fatalf("e should return a set-enabled cmd")
	}
	cmd()
	fa := m.api.(*fakeAPI)
	if fa.enabledID != "codex" {
		t.Fatalf("want enabledID=codex, got %q", fa.enabledID)
	}
	if !fa.enabledVal {
		t.Fatalf("a disabled backend should toggle to enabled=true")
	}
}

func TestBackendsRescan(t *testing.T) {
	m := backendsModel(0)
	_, cmd := m.handleKey(key("r"))
	if cmd == nil {
		t.Fatalf("r should return a rescan cmd")
	}
	cmd()
	if !m.api.(*fakeAPI).rescanned {
		t.Fatalf("r should call RescanBackends")
	}
}

func TestBackendsToggleThinkingMode(t *testing.T) {
	m := backendsModel(0) // free_plus_local
	_, cmd := m.handleKey(key("m"))
	if cmd == nil {
		t.Fatalf("m should return a set-thinking-mode cmd")
	}
	cmd()
	fa := m.api.(*fakeAPI)
	if fa.thinkingMode != backendstore.ThinkingModeLocalOnly {
		t.Fatalf("free_plus_local should toggle to local_only, got %q", fa.thinkingMode)
	}
}

func TestBackendsClose(t *testing.T) {
	for _, k := range []string{"esc", "b"} {
		m := backendsModel(0)
		updated, _ := m.handleKey(key(k))
		if got := updated.(controlPaneModel).mode; got != modeNormal {
			t.Fatalf("%q should return to modeNormal, got %v", k, got)
		}
	}
}

func TestBackendsBodyRendersTableAndLocalRow(t *testing.T) {
	body := backendsBody(backendsFixture(), 0)
	for _, want := range []string{
		"internal thinking", "free_plus_local", // header control
		"ID", "TIER", "DEF", "EN", "LIMITED", // column headers
		"claude", "codex", "local", // every row incl. the reserved local row
		"›", // the cursor marker on the selected row
	} {
		if !strings.Contains(body, want) {
			t.Errorf("backends body missing %q:\n%s", want, body)
		}
	}
}

func TestBackendsBodyLimitedCountdown(t *testing.T) {
	st := backendsFixture()
	st.Backends[1].LimitedUntil = time.Now().Add(90 * time.Second)
	body := backendsBody(st, 0)
	if !strings.Contains(body, "1m30s") {
		t.Errorf("a rate-limited backend should show its remaining time:\n%s", body)
	}
}

func TestNextTierCycle(t *testing.T) {
	cases := map[string]string{
		backendstore.TierFree:         backendstore.TierSubscription,
		backendstore.TierSubscription: backendstore.TierPayPerUse,
		backendstore.TierPayPerUse:    backendstore.TierUnclassified,
		backendstore.TierUnclassified: backendstore.TierFree,
		"something-unknown":           backendstore.TierFree, // off-cycle → first entry
	}
	for cur, want := range cases {
		if got := nextTier(cur); got != want {
			t.Errorf("nextTier(%q) = %q, want %q", cur, got, want)
		}
	}
}

func TestNextThinkingMode(t *testing.T) {
	if got := nextThinkingMode(backendstore.ThinkingModeFreePlusLocal); got != backendstore.ThinkingModeLocalOnly {
		t.Errorf("free_plus_local should toggle to local_only, got %q", got)
	}
	if got := nextThinkingMode(backendstore.ThinkingModeLocalOnly); got != backendstore.ThinkingModeFreePlusLocal {
		t.Errorf("local_only should toggle to free_plus_local, got %q", got)
	}
}
