package tui

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func fallbackFleet() ([]projectstore.Project, []*store.Session) {
	return []projectstore.Project{{ID: "proj", Path: "/repo", Name: "project", Status: projectstore.StatusOpen}}, []*store.Session{
		{ID: "worker", ProjectID: "proj", Repo: "/repo", Status: store.StatusIdle, Tags: []string{"run:ap-1"}},
		{ID: "manager", Name: "manager name", ProjectID: "proj", Repo: "/repo", Status: store.StatusWorking, Role: "autopilot", Tags: []string{"system:true", "run:ap-1"}},
	}
}

func TestAutopilotFallbackInitialFailureAndRecovery(t *testing.T) {
	projects, sessions := fallbackFleet()
	m := newListPane(&fakeAPI{}, "%9", "")
	m.projects, m.sessions = projects, sessions
	m = lstep(m, autopilotMsg{err: context.DeadlineExceeded})
	m = lstep(m, tea.WindowSizeMsg{Width: 160, Height: 40})
	items := m.items()
	runIndex := -1
	for i, it := range items {
		if it.apRun != nil {
			runIndex = i
			require.Equal(t, "ap-1", it.apRun.RunID)
			require.Equal(t, autopilotStatusUnavailable, it.apRun.State)
			line := renderItemLine(it, false, 160)
			require.Contains(t, line, "status unavailable")
			require.NotContains(t, line, "0/0 tasks")
		}
	}
	require.GreaterOrEqual(t, runIndex, 0)
	require.Greater(t, itemIndexBySessionID(items, "manager"), runIndex)
	require.Greater(t, itemIndexBySessionID(items, "worker"), runIndex)
	require.Equal(t, store.AutopilotSlotManager, items[itemIndexBySessionID(items, "manager")].apSlot)
	require.Equal(t, store.AutopilotSlotWorker, items[itemIndexBySessionID(items, "worker")].apSlot)
	require.Contains(t, m.View(), "autopilot: status unavailable")
	require.Empty(t, m.autopilot.Runs, "fallback is presentation-only")

	// Actual success replaces inferred identity/state without duplicate nodes.
	m = lstep(m, autopilotMsg{status: client.AutopilotStatus{Runs: []client.AutopilotRunStatus{{RunID: "ap-1", Name: "real run", Repo: "/repo", State: "active"}}}})
	require.NoError(t, m.autopilotErr)
	count := 0
	for _, it := range m.items() {
		if it.apRun != nil {
			count++
			require.Equal(t, "active", it.apRun.State)
			require.Equal(t, "real run", it.apRun.Name)
		}
	}
	require.Equal(t, 1, count)
	require.NotContains(t, m.View(), "status unavailable")
}

func TestAutopilotFallbackCachedStateIsNotCurrentOrActionable(t *testing.T) {
	projects, sessions := fallbackFleet()
	f := &fakeAPI{}
	m := newListPane(f, "%9", "")
	m.projects, m.sessions = projects, sessions
	st := client.AutopilotStatus{Runs: []client.AutopilotRunStatus{{RunID: "ap-1", Name: "release", PlanID: "plan-1", Repo: "/repo", State: "awaiting_merge", FinalPR: &client.AutopilotFinalPR{Number: 99}}}}
	m = lstep(m, autopilotMsg{status: st})
	m = lstep(m, autopilotMsg{err: context.DeadlineExceeded})
	require.Equal(t, "awaiting_merge", m.autopilot.Runs[0].State, "do not mutate the cached API response")
	for i, it := range m.items() {
		if it.apRun != nil {
			m.cursor = i
			require.Equal(t, autopilotStatusUnavailable, it.apRun.State)
			require.NotContains(t, renderItemLine(it, false, 200), "awaiting final PR")
		}
	}
	for _, k := range []string{"r", "x", "enter"} {
		nm, cmd := m.Update(key(k))
		require.Nil(t, cmd, "%s must not act using unavailable run state", k)
		require.Contains(t, nm.(controlPaneModel).status, "status unavailable")
	}
	require.Empty(t, f.autopilotAction)
	// Children still expose their normal attach action.
	m.cursor = itemIndexBySessionID(m.items(), "worker")
	_, cmd := m.Update(key("enter"))
	require.NotNil(t, cmd)
}

func TestAutopilotFallbackOwnershipAndHistory(t *testing.T) {
	for _, ownership := range []string{"backref", "run:", "autopilot-run:"} {
		t.Run(ownership, func(t *testing.T) {
			projects, sessions := fallbackFleet()
			sessions = sessions[:1] // worker-only run, no manager required
			sessions[0].Tags = nil
			if ownership == "backref" {
				sessions[0].AutopilotRunID = "ap-1"
			} else {
				sessions[0].Tags = []string{ownership + "ap-1"}
			}
			sessions[0].Status = store.StatusRateLimited
			plans := map[string][]*planstore.Plan{"proj": {{ID: "plan-1", Name: "Linked plan", AutopilotRunID: "ap-1"}}}
			st := autopilotWithSessionFallback(client.AutopilotStatus{}, sessions, projects, plans)
			require.Len(t, st.Runs, 1)
			require.Equal(t, "Linked plan", st.Runs[0].Name)
			require.Equal(t, "plan-1", st.Runs[0].PlanID)
			require.Equal(t, "/repo", st.Runs[0].Repo)
			for _, status := range []store.Status{store.StatusDone, store.StatusErrored, store.StatusOrphaned} {
				sessions[0].Status = status
				require.Empty(t, autopilotWithSessionFallback(client.AutopilotStatus{}, sessions, projects, plans).Runs)
			}
		})
	}
}

func TestAutopilotFallbackMixedRunsAndArrivalOrder(t *testing.T) {
	projects, sessions := fallbackFleet()
	// A different project/run must not be swallowed or borrow the first name.
	projects = append(projects, projectstore.Project{ID: "other", Path: "/other", Name: "Other", Status: projectstore.StatusOpen})
	sessions = append(sessions, &store.Session{ID: "other-worker", ProjectID: "other", Workdir: "/other/.worktrees/task", Status: store.StatusIdle, AutopilotRunID: "ap-2"})
	st := client.AutopilotStatus{Runs: []client.AutopilotRunStatus{{RunID: "ap-1", Name: "real", Repo: "/repo", State: "active"}}}
	for _, ss := range [][]*store.Session{sessions, {sessions[2], sessions[1], sessions[0]}} {
		out := autopilotWithSessionFallback(st, ss, projects, nil)
		require.Len(t, out.Runs, 2)
		require.Equal(t, "active", out.Runs[0].State)
		require.Equal(t, "ap-2", out.Runs[1].Name)
		require.Equal(t, "/other", out.Runs[1].Repo)
		require.Len(t, st.Runs, 1)
	}
	// Even a successful empty status response can race with session discovery.
	items := buildProjectItems(projects, nil, sessions, nil, client.AutopilotStatus{}, nil, nil, nil, false)
	for _, id := range []string{"manager", "worker", "other-worker"} {
		require.GreaterOrEqual(t, itemIndexBySessionID(items, id), 0)
	}
	// Once authoritative sessions also disappear, inferred rows disappear too.
	require.Empty(t, autopilotWithSessionFallback(client.AutopilotStatus{}, nil, projects, nil).Runs)
}
