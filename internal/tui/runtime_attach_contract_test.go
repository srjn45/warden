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

	t.Run("direct runtime currently selects a tmux-only viewer command", func(t *testing.T) {
		attach, _, _, detail := cockpitDetailCmd(item{session: direct})
		require.Equal(t, direct.ID, attach, "the decision layer keeps the direct record addressable by stable id")
		require.Empty(t, detail)

		// This is an executable characterization of the current gap, rather than
		// a ceremonial skip: opening that stable id still renders a nested tmux
		// attach command.  A direct record deliberately has no TmuxSession, so
		// this command cannot attach it.  02b-02 must replace this with the shared
		// `warden attach <id>` client and surface ErrUnknownRuntime explicitly.
		args := respawnDetailArgs("%7", attach)
		require.Equal(t, "env -u TMUX tmux attach -t agent-direct", args[len(args)-1])
		require.NotContains(t, args[len(args)-1], "warden attach")
	})

	t.Run("unknown runtime is still routed to the tmux-only viewer", func(t *testing.T) {
		attach, _, _, detail := cockpitDetailCmd(item{session: unknown})
		require.Equal(t, unknown.ID, attach)
		require.Empty(t, detail)
		require.Equal(t, "env -u TMUX tmux attach -t agent-unknown", respawnDetailArgs("%7", attach)[4])
	})
}
