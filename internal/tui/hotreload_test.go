package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/updater"
)

func TestUpdateChipText(t *testing.T) {
	t.Parallel()
	require.Equal(t, "", updateChipText(updateState{LocalVersion: "1.0.0"}))
	require.Equal(t,
		"[u] Update to v1.2.3 available (press 'u')",
		updateChipText(updateState{LocalVersion: "1.0.0", AvailableVersion: "1.2.3"}),
	)
	require.Equal(t,
		"[r] Warden upgraded to v2.0.0 — press 'r' to reload TUI",
		updateChipText(updateState{LocalVersion: "1.0.0", DaemonVersion: "v2.0.0", AvailableVersion: "9.9.9"}),
	)
}

func TestRouteHotReloadKey(t *testing.T) {
	t.Parallel()
	avail := updateState{LocalVersion: "1.0.0", AvailableVersion: "1.1.0"}
	reload := updateState{LocalVersion: "1.0.0", DaemonVersion: "1.1.0"}
	both := updateState{LocalVersion: "1.0.0", DaemonVersion: "1.1.0", AvailableVersion: "1.2.0"}

	require.Equal(t, hotReloadKeyPromptUpdate, routeHotReloadKey("u", avail))
	require.Equal(t, hotReloadKeyNone, routeHotReloadKey("u", reload))
	require.Equal(t, hotReloadKeyNone, routeHotReloadKey("u", both)) // reload wins; u is inactive
	require.Equal(t, hotReloadKeyReload, routeHotReloadKey("r", reload))
	require.Equal(t, hotReloadKeyNone, routeHotReloadKey("r", avail))
	require.Equal(t, hotReloadKeyNone, routeHotReloadKey("x", avail))
}

func TestVersionEqualStripsV(t *testing.T) {
	t.Parallel()
	require.True(t, versionEqual("v1.2.3", "1.2.3"))
	require.False(t, versionEqual("1.2.3", "1.2.4"))
}

func TestKeyUOpensUpdateConfirm(t *testing.T) {
	m := newListPane(&fakeAPI{}, "", "")
	m.ready = true
	m.w, m.h = 80, 24
	m.localVersion = "1.0.0"
	m.availableVersion = "1.2.0"

	nm, cmd := m.Update(key("u"))
	um := nm.(controlPaneModel)
	require.Equal(t, modeConfirmUpdate, um.mode)
	require.Nil(t, cmd)

	view := um.View()
	require.Contains(t, view, "Update warden to v1.2.0")
}

func TestKeyRReloadsWhenDaemonAhead(t *testing.T) {
	m := newListPane(&fakeAPI{}, "", "")
	m.ready = true
	m.localVersion = "1.0.0"
	m.daemonVersion = "1.1.0"

	nm, cmd := m.Update(key("r"))
	um := nm.(controlPaneModel)
	require.True(t, um.pendingExec)
	require.NotNil(t, cmd)
	// reloadQuitCmd is tea.Quit — drain once.
	msg := cmd()
	_, isQuit := msg.(tea.QuitMsg)
	require.True(t, isQuit, "expected tea.QuitMsg, got %T", msg)
}

func TestKeyRKeepsRestoreWhenNoReload(t *testing.T) {
	// Without a version mismatch, `r` must not claim the key for reload.
	require.Equal(t, hotReloadKeyNone, routeHotReloadKey("r", updateState{LocalVersion: "1.0.0"}))
}

func TestHealthMsgUpdatesDaemonVersion(t *testing.T) {
	m := newListPane(&fakeAPI{}, "", "")
	m.localVersion = "1.0.0"
	nm, _ := m.Update(healthMsg{version: "2.0.0"})
	um := nm.(controlPaneModel)
	require.Equal(t, "2.0.0", um.daemonVersion)
	require.Contains(t, updateChipText(um.updateState()), "press 'r' to reload")
}

func TestUpdateCheckMsgSetsAvailable(t *testing.T) {
	m := newListPane(&fakeAPI{}, "", "")
	m.localVersion = "1.0.0"
	nm, _ := m.Update(updateCheckMsg{available: "1.5.0"})
	um := nm.(controlPaneModel)
	require.Equal(t, "1.5.0", um.availableVersion)
	require.Contains(t, updateChipText(um.updateState()), "press 'u'")
}

func TestConfirmUpdateApplies(t *testing.T) {
	origCheck, origApply := checkUpdateFn, applyUpdateFn
	t.Cleanup(func() {
		checkUpdateFn, applyUpdateFn = origCheck, origApply
	})
	applyUpdateFn = func(current string) (updater.Result, error) {
		require.Equal(t, "1.0.0", current)
		return updater.Result{TargetVersion: "1.2.0", Updated: true, Message: "updated"}, nil
	}

	m := newListPane(&fakeAPI{}, "", "")
	m.mode = modeConfirmUpdate
	m.localVersion = "1.0.0"
	m.availableVersion = "1.2.0"

	nm, cmd := m.Update(key("y"))
	um := nm.(controlPaneModel)
	require.Equal(t, modeNormal, um.mode)
	require.Contains(t, um.status, "updating")
	require.NotNil(t, cmd)

	msg := cmd()
	applyMsg, ok := msg.(updateApplyMsg)
	require.True(t, ok)
	require.NoError(t, applyMsg.err)
	require.Equal(t, "1.2.0", applyMsg.target)

	nm2, cmd2 := um.Update(applyMsg)
	um2 := nm2.(controlPaneModel)
	require.True(t, um2.pendingExec)
	require.NotNil(t, cmd2)
}

func TestFooterShowsUpdateChip(t *testing.T) {
	m := newListPane(&fakeAPI{}, "", "")
	m.ready = true
	m.w, m.h = 100, 30
	m.localVersion = "1.0.0"
	m.availableVersion = "1.4.0"
	view := m.View()
	require.Contains(t, view, "[u] Update to v1.4.0 available (press 'u')")
}
