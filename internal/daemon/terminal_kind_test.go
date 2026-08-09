package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// These tests pin the stage-2 invariant: a Kind==terminal session is a plain
// shell and must be excluded from every AI-centric aggregation, while an
// agent-kind session (empty or "agent") is unaffected.

func TestLiveAgentsExcludesTerminals(t *testing.T) {
	fs := newFakeStore()
	fs.data["agent-1"] = &store.Session{ID: "agent-1", TmuxSession: "agent-1", Status: store.StatusWorking}
	fs.data["term-1"] = &store.Session{ID: "term-1", TmuxSession: "term-1", Status: store.StatusWorking, Kind: store.KindTerminal}

	agents, err := storeAgentLister{st: fs}.LiveAgents(context.Background())
	require.NoError(t, err)
	require.Len(t, agents, 1, "terminal must not be attributed in fleet metrics")
	require.Equal(t, "agent-1", agents[0].ID)
}

func TestLiveAgentCountExcludesTerminals(t *testing.T) {
	fs := newFakeStore()
	fs.data["agent-1"] = &store.Session{ID: "agent-1", Status: store.StatusWorking}
	fs.data["agent-2"] = &store.Session{ID: "agent-2", Status: store.StatusIdle}
	fs.data["term-1"] = &store.Session{ID: "term-1", Status: store.StatusWorking, Kind: store.KindTerminal}

	s := &Server{store: fs}
	require.Equal(t, 2, s.liveAgentCount(context.Background()),
		"the spawn-gate count must budget AI agents only, not terminals")
}

func TestListApprovalsExcludesTerminals(t *testing.T) {
	// Both sessions are parked at waiting_for_input; only the agent may surface an
	// approval — a terminal has no yes/no prompt warden answers.
	pane := "│ Do you want to proceed?\n│ ❯ 1. Yes\n│   2. No\n"
	fs := newFakeStore()
	fs.data["agent-1"] = &store.Session{ID: "agent-1", TmuxSession: "agent-1", Status: store.StatusWaitingForInput, LastPaneExcerpt: pane}
	fs.data["term-1"] = &store.Session{ID: "term-1", TmuxSession: "term-1", Status: store.StatusWaitingForInput, LastPaneExcerpt: pane, Kind: store.KindTerminal}

	s := &Server{store: fs, approvals: true}
	resp, err := s.ListApprovals(context.Background(), oapi.ListApprovalsRequestObject{})
	require.NoError(t, err)
	out, ok := resp.(oapi.ListApprovals200JSONResponse)
	require.True(t, ok)
	require.True(t, out.Enabled)
	require.Len(t, out.Approvals, 1, "terminal must not enter the approvals queue")
	require.Equal(t, "agent-1", out.Approvals[0].ID)
}

func TestFilterClosedExcludesTerminals(t *testing.T) {
	now := time.Now()
	in := []*store.Session{
		{ID: "agent-1", UpdatedAt: now, Type: store.TypeDevelopment},
		{ID: "term-1", UpdatedAt: now, Kind: store.KindTerminal},
	}
	out := filterClosed(in, time.Time{}, "", 0)
	require.Len(t, out, 1, "history is an AI-agent record; terminals are dropped")
	require.Equal(t, "agent-1", out[0].ID)
}
