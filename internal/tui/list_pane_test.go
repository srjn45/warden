package tui

import (
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func lstep(m listPaneModel, msg tea.Msg) listPaneModel {
	nm, _ := m.Update(msg)
	return nm.(listPaneModel)
}

func TestListPaneGroupsBySourceDir(t *testing.T) {
	now := time.Now()
	m := newListPane(&fakeAPI{}, "%9")
	m = lstep(m, sessionsMsg{sessions: []*store.Session{
		{ID: "b1", Workdir: "/b", UpdatedAt: now.Add(-1 * time.Minute)},
		{ID: "a1", Workdir: "/a", UpdatedAt: now.Add(-2 * time.Minute)},
		{ID: "b2", Workdir: "/b", UpdatedAt: now.Add(-3 * time.Minute)},
	}})
	ids := []string{m.sessions[0].ID, m.sessions[1].ID, m.sessions[2].ID}
	require.Equal(t, []string{"b1", "b2", "a1"}, ids, "cockpit list pane stores grouped order")
}

func TestListPaneNewAgentNameFieldFlowsToSpawn(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9")
	m = lstep(m, key("n"))
	require.Equal(t, modeNewAgent, m.mode)
	// ctrl+n focuses the name field; type a name, then enter returns to the prompt.
	m = lstep(m, tea.KeyMsg{Type: tea.KeyCtrlN})
	require.Equal(t, modeNewAgentName, m.mode)
	m = lstep(m, key("my-agent"))
	m = lstep(m, key("enter"))
	require.Equal(t, modeNewAgent, m.mode)
	// ctrl+s submits the spawn carrying the typed name.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	require.NotNil(t, cmd)
	cmd()
	require.NotNil(t, f.spawned)
	require.Equal(t, "my-agent", f.spawned.Name)
}

func TestListPaneRenameFromDetails(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9")
	m = lstep(m, sessionsMsg{sessions: []*store.Session{{ID: "a1", Name: "old", Workdir: "/w"}}})
	m = lstep(m, key("i")) // open details
	require.Equal(t, modeDetails, m.mode)
	m = lstep(m, key("r")) // start rename
	require.Equal(t, modeRename, m.mode)
	require.Equal(t, "old", m.tn.Value(), "rename seeds the current name")
	require.Equal(t, "a1", m.renameID)
	// Clear and type a new name, then enter to submit.
	m.tn.SetValue("new-name")
	nm, cmd := m.Update(key("enter"))
	m = nm.(listPaneModel)
	require.Equal(t, modeDetails, m.mode)
	require.NotNil(t, cmd)
	cmd()
	require.Equal(t, "a1", f.renamedID)
	require.Equal(t, "new-name", f.renamedName)
}

func TestListPaneRenameEscCancels(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9")
	m = lstep(m, sessionsMsg{sessions: []*store.Session{{ID: "a1", Name: "old", Workdir: "/w"}}})
	m = lstep(m, key("i"))
	m = lstep(m, key("r"))
	require.Equal(t, modeRename, m.mode)
	m = lstep(m, key("esc"))
	require.Equal(t, modeDetails, m.mode)
	require.Empty(t, f.renamedID, "esc cancels the rename without calling SetName")
}

func TestListPaneDeletePipelineConfirmsThenDeletes(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9")
	m = lstep(m, pipelinesMsg{pipelines: donePipeline()})
	require.NotNil(t, itemAt(m.items(), m.cursor).pipeline)
	m = lstep(m, key("D"))
	require.Equal(t, modeConfirmDeletePipeline, m.mode)
	_, cmd := m.Update(key("y"))
	require.NotNil(t, cmd)
	cmd() // drives fakeAPI.PipelineDelete
	require.Equal(t, "demo", f.deletedPipe)
}

func TestListPaneDeletePipelineRefusedWhenLive(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9")
	m = lstep(m, pipelinesMsg{pipelines: []*pipeline.Pipeline{
		{ID: "demo", Status: pipeline.StatusRunning, Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobRunning}}},
	}})
	m = lstep(m, key("D"))
	require.NotEqual(t, modeConfirmDeletePipeline, m.mode)
	require.Empty(t, f.deletedPipe)
	require.Contains(t, m.status, "cancel")
}

func TestListPaneSpawnModal(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9")
	m = lstep(m, key("n"))
	require.Equal(t, modeNewAgent, m.mode)
	m = lstep(m, key("esc"))
	require.Equal(t, modeNormal, m.mode)
}

func TestListPaneEmptyPromptSpawnsInteractive(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9")
	m = lstep(m, key("n"))
	require.Equal(t, modeNewAgent, m.mode)
	// Submit immediately, without typing a prompt — the cockpit's own Ctrl+S
	// handler must also treat a blank prompt as an interactive spawn.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	require.NotNil(t, cmd)
	cmd() // drives fakeAPI.Spawn
	require.NotNil(t, f.spawned, "cockpit: empty prompt spawns an interactive agent")
	require.Equal(t, "", f.spawned.Prompt)
}

func TestListPaneEnterOpensDetail(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9")
	m = lstep(m, sessionsMsg{sessions: []*store.Session{{ID: "a", TmuxSession: "a"}}})
	_, cmd := m.Update(key("enter"))
	require.NotNil(t, cmd, "Enter on a selected agent opens it in the detail pane")
}

func TestListPaneEnterNoopWithoutSelection(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9")
	_, cmd := m.Update(key("enter")) // no sessions loaded
	require.Nil(t, cmd, "Enter with no selection does nothing")
}

func TestListPaneOpenDirAddsPlaceholder(t *testing.T) {
	f := &fakeAPI{dirListing: client.DirListing{Path: "/work/api"}}
	m := newListPane(f, "%9")
	m = lstep(m, key("o"))
	require.Equal(t, modeOpenDir, m.mode)
	m.tp.SetValue("/work/api")
	_, cmd := m.Update(key("enter"))
	require.NotNil(t, cmd, "enter dispatches openDirCmd")
	m = lstep(m, openDirMsg{dir: "/work/api"}) // the validated result
	require.Equal(t, modeNormal, m.mode)
	items := m.items()
	require.Len(t, items, 1)
	require.Nil(t, items[0].session)
	require.Equal(t, "/work/api", items[0].dir)
}

func TestListPaneNewAgentResolvesTargetDir(t *testing.T) {
	now := time.Now()
	m := newListPane(&fakeAPI{}, "%9")
	m = lstep(m, sessionsMsg{sessions: []*store.Session{{ID: "a1", Workdir: "/work/api", UpdatedAt: now}}})
	m = lstep(m, key("n"))
	require.Equal(t, modeNewAgent, m.mode)
	require.Equal(t, "/work/api", m.targetDir, "new agent defaults to the cursor group's dir")
}

func TestListPaneCloseOpenedDirWithX(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9")
	m.openedDirs["/work/empty"] = time.Now()
	require.Len(t, m.items(), 1)
	m = lstep(m, key("x")) // cursor is on the placeholder
	require.Empty(t, m.openedDirs, "x on a placeholder closes the opened dir")
	require.NotEqual(t, modeConfirmKill, m.mode, "no kill-confirm for a placeholder")
}

func TestListPaneXOnAgentStillConfirms(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9")
	m = lstep(m, sessionsMsg{sessions: []*store.Session{{ID: "a1", Workdir: "/work/api"}}})
	m = lstep(m, key("x"))
	require.Equal(t, modeConfirmKill, m.mode)
}

func TestRespawnDetailArgs(t *testing.T) {
	require.Equal(t,
		[]string{"respawn-pane", "-k", "-t", "%9", "env -u TMUX tmux attach -t agent-4f98"},
		respawnDetailArgs("%9", "agent-4f98"))
}

func TestListPaneCollapsesCompletedPipelinesByDefault(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9")
	m = lstep(m, pipelinesMsg{pipelines: []*pipeline.Pipeline{
		{ID: "done1", Status: pipeline.StatusDone, Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobDone}}},
		{ID: "cancel1", Status: pipeline.StatusCanceled, Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobDone}}},
		{ID: "run1", Status: pipeline.StatusRunning, Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobRunning}}},
	}})
	require.True(t, m.collapsed["done1"], "done pipeline collapsed by default")
	require.True(t, m.collapsed["cancel1"], "canceled pipeline collapsed by default")
	require.False(t, m.collapsed["run1"], "running pipeline stays expanded")
}

func TestListPaneRespectsManualExpandAcrossRefresh(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9")
	pipes := []*pipeline.Pipeline{
		{ID: "done1", Status: pipeline.StatusDone, Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobDone}}},
	}
	m = lstep(m, pipelinesMsg{pipelines: pipes})
	require.True(t, m.collapsed["done1"], "completed pipeline starts collapsed")
	m.collapsed["done1"] = false // user expands it
	m = lstep(m, pipelinesMsg{pipelines: pipes})
	require.False(t, m.collapsed["done1"], "manual expand survives refresh; not re-collapsed")
}

func TestListPanePipelinesAndCancel(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9")
	m = lstep(m, pipelinesMsg{pipelines: []*pipeline.Pipeline{
		{ID: "demo", Name: "demo", Status: pipeline.StatusRunning,
			Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobFailed, SessionID: "demo-a"}}},
	}})
	if itemAt(m.items(), 0).pipeline == nil {
		t.Fatalf("cockpit list should show the pipeline header first")
	}
	m.cursor = 0
	_, cmd := m.Update(key("x"))
	if cmd == nil {
		t.Fatalf("x on a pipeline row should return a cancel cmd")
	}
	cmd()
	if got := m.api.(*fakeAPI).canceled; got != "demo" {
		t.Fatalf("want canceled=demo, got %q", got)
	}
}

func TestCockpitDetailCmdTerminalJobRendersDetail(t *testing.T) {
	at, jp, jid := cockpitDetailCmd(item{pjPipe: "pl", pjJob: &pipeline.Job{
		ID: "only", Status: pipeline.JobDone, SessionID: "pl-only"}})
	require.Equal(t, "", at, "terminal job must not attach to its dead tmux session")
	require.Equal(t, "pl", jp)
	require.Equal(t, "only", jid)
}

func TestCockpitDetailCmdRunningJobAttaches(t *testing.T) {
	at, jp, jid := cockpitDetailCmd(item{pjPipe: "pl", pjJob: &pipeline.Job{
		ID: "b", Status: pipeline.JobRunning, SessionID: "pl-b"}})
	require.Equal(t, "pl-b", at)
	require.Equal(t, "", jp+jid)
}

func TestCockpitDetailCmdLiveAgentAttaches(t *testing.T) {
	at, jp, jid := cockpitDetailCmd(item{session: &store.Session{ID: "x", TmuxSession: "x"}})
	require.Equal(t, "x", at)
	require.Equal(t, "", jp+jid)
}

func TestCockpitDetailCmdPipelineHeaderShowsNothing(t *testing.T) {
	at, jp, jid := cockpitDetailCmd(item{pipeline: &pipeline.Pipeline{ID: "pl"}})
	require.Equal(t, "", at+jp+jid)
}

func TestRespawnJobDetailArgs(t *testing.T) {
	require.Equal(t,
		[]string{"respawn-pane", "-k", "-t", "%3",
			"/usr/bin/warden tui --pane=jobdetail --pipeline=pl --job=only"},
		respawnJobDetailArgs("%3", "/usr/bin/warden", "pl", "only"))
}

func TestListPaneInspectorTogglesAndFetches(t *testing.T) {
	f := &fakeAPI{
		ctxEntries: []client.ContextEntry{{Key: "global.k", Value: "v"}},
		messages:   []client.Message{{From: "a", To: "b", Body: "hi"}},
	}
	m := newListPane(f, "%9")
	m = lstep(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	// `c` opens the inspector and fires the read-only fetch commands.
	nm, cmd := m.Update(key("c"))
	m = nm.(listPaneModel)
	require.Equal(t, modeInspector, m.mode)

	// Run the fetch commands and feed their results back into the model.
	for _, msg := range collectFast(cmd) {
		m = lstep(m, msg)
	}
	require.Equal(t, inspectorMsgLimit, f.msgLimit, "MsgRecent should be called with the inspector limit")

	view := m.View()
	require.Contains(t, view, "Context & Messages")
	require.Contains(t, view, "global.k")
	require.Contains(t, view, "hi")

	// `esc` returns to the normal list view.
	m = lstep(m, key("esc"))
	require.Equal(t, modeNormal, m.mode)
}

func TestListPaneInspectorScrollAndRefreshPreservesOffset(t *testing.T) {
	// Enough context entries to overflow the viewport so scrolling is meaningful.
	var entries []client.ContextEntry
	for i := 0; i < 60; i++ {
		entries = append(entries, client.ContextEntry{Key: fmt.Sprintf("global.k%02d", i), Value: "v"})
	}
	f := &fakeAPI{ctxEntries: entries}
	m := newListPane(f, "%9")
	m = lstep(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	// Open the inspector and load the (large) context snapshot.
	m = lstep(m, key("c"))
	m = lstep(m, contextMsg{entries: entries})
	require.Equal(t, 0, m.vp.YOffset, "a freshly opened inspector starts at the top")

	// Scroll down a few lines.
	for i := 0; i < 5; i++ {
		m = lstep(m, key("down"))
	}
	scrolled := m.vp.YOffset
	require.Greater(t, scrolled, 0, "↓ should scroll the inspector viewport")

	// A refresh tick (new contextMsg) must NOT snap back to the top.
	m = lstep(m, contextMsg{entries: entries})
	require.Equal(t, scrolled, m.vp.YOffset, "refresh must preserve the scroll offset")

	// G jumps to the bottom; g returns to the top.
	m = lstep(m, key("G"))
	require.Greater(t, m.vp.YOffset, scrolled, "G should jump toward the bottom")
	m = lstep(m, key("g"))
	require.Equal(t, 0, m.vp.YOffset, "g should jump back to the top")

	// Re-opening the inspector resets to the top.
	for i := 0; i < 5; i++ {
		m = lstep(m, key("down"))
	}
	m = lstep(m, key("esc"))
	m = lstep(m, key("c"))
	require.Equal(t, 0, m.vp.YOffset, "re-opening the inspector resets to the top")
}

func TestListPaneInspectorTickRefreshesOnlyWhenOpen(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9")

	// A tick in normal mode must not fetch context/messages.
	_, cmd := m.Update(tickMsg(time.Now()))
	ctx, msg := batchHasInspectorFetches(collectFast(cmd))
	require.False(t, ctx || msg, "normal-mode tick must not fetch context/messages")

	// Once the inspector is open, the tick batch includes the inspector fetches.
	m.mode = modeInspector
	_, cmd = m.Update(tickMsg(time.Now()))
	ctx, msg = batchHasInspectorFetches(collectFast(cmd))
	require.True(t, ctx, "inspector tick should refresh context")
	require.True(t, msg, "inspector tick should refresh messages")
}

// collectFast runs a (possibly batched) command and returns the messages from
// every sub-command that completes quickly, dropping slow ones like tick()
// (which sleeps a second before emitting). The fake api's commands return
// immediately, so the fetches we care about are always captured.
func collectFast(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	first := cmd()
	batch, ok := first.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{first}
	}
	ch := make(chan tea.Msg, len(batch))
	n := 0
	for _, c := range batch {
		if c == nil {
			continue
		}
		n++
		go func(c tea.Cmd) { ch <- c() }(c)
	}
	deadline := time.After(500 * time.Millisecond)
	var out []tea.Msg
	for i := 0; i < n; i++ {
		select {
		case m := <-ch:
			out = append(out, m)
		case <-deadline:
			return out
		}
	}
	return out
}

func batchHasInspectorFetches(msgs []tea.Msg) (ctx, msg bool) {
	for _, m := range msgs {
		switch m.(type) {
		case contextMsg:
			ctx = true
		case messagesMsg:
			msg = true
		}
	}
	return ctx, msg
}
