package tui

import (
	"net/http"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// TestRuntimeAttachContractCharacterization is a bounded, green characterization
// fixture for 02b-02's entry state. It deliberately does not execute tmux: the
// next task replaces the pane's nested tmux attach with the shared client.
func TestRuntimeAttachContractCharacterization(t *testing.T) {
	direct := &store.Session{
		ID: "agent-direct", Status: store.StatusWorking,
		Runtime: &store.SessionRuntime{Kind: "direct", PinnedMode: "on"},
	}
	legacy := &store.Session{
		ID: "agent-legacy", Status: store.StatusWorking, TmuxSession: "legacy-pane",
		Runtime: &store.SessionRuntime{Kind: "tmux", PinnedMode: "shadow"},
	}
	unknown := &store.Session{
		ID: "agent-unknown", Status: store.StatusWorking,
		Runtime: &store.SessionRuntime{Kind: "unknown"},
	}

	t.Run("direct absent from tmux has a stable selection identity", func(t *testing.T) {
		m := newListPane(&fakeAPI{}, "%9", "%1")
		m = lstep(m, sessionsMsg{sessions: []*store.Session{direct, legacy, unknown}})
		m.cursor = cursorOn(m, func(it item) bool { return it.session != nil && it.session.ID == direct.ID })
		require.Equal(t, direct.ID, m.selectedID())
		attach, _, _, detail := cockpitDetailCmd(item{session: direct})
		require.Equal(t, direct.ID, attach, "direct records without tmux remain addressable by record id")
		require.Empty(t, detail)
	})

	t.Run("first status error creates no phantom fleet or selection", func(t *testing.T) {
		m := newListPane(&fakeAPI{}, "%9", "%1")
		m = lstep(m, sessionsMsg{err: &client.StatusError{Code: http.StatusServiceUnavailable, Msg: "store degraded"}})
		require.Empty(t, m.sessions)
		require.Empty(t, m.selectedID())
	})

	t.Run("mixed fleet survives degradation and a removed selection is not retained", func(t *testing.T) {
		m := newListPane(&fakeAPI{}, "%9", "%1")
		m = lstep(m, sessionsMsg{sessions: []*store.Session{direct, legacy, unknown}})
		m.cursor = cursorOn(m, func(it item) bool { return it.session != nil && it.session.ID == legacy.ID })
		require.Equal(t, legacy.ID, m.selectedID())

		m = lstep(m, sessionsMsg{err: &client.StatusError{Code: http.StatusServiceUnavailable, Msg: "store degraded"}})
		require.Equal(t, legacy.ID, m.selectedID(), "last complete selection survives a non-authoritative error")
		require.Len(t, m.sessions, 3)

		// A complete snapshot is authoritative: it may remove the selected record,
		// but must not leave that absent id selected.
		fresh := &store.Session{ID: "agent-fresh", Status: store.StatusWorking, UpdatedAt: time.Now()}
		m = lstep(m, sessionsMsg{sessions: []*store.Session{direct, fresh}})
		require.NotEqual(t, legacy.ID, m.selectedID(), "selection must not remain stale after a complete refresh")
	})

	t.Run("unknown runtime attach is an explicit 02b-02 gap", func(t *testing.T) {
		_, _, _, _ = cockpitDetailCmd(item{session: unknown})
		t.Skip("02b-02 must replace the current nested tmux attach with ptyattach.Client and show ErrUnknownRuntime")
	})
}
