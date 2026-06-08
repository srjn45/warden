package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srajanpathak/agentctl/internal/client"
	"github.com/srajanpathak/agentctl/internal/pipeline"
	"github.com/srajanpathak/agentctl/internal/store"
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
			"/usr/bin/agentctl tui --pane=jobdetail --pipeline=pl --job=only"},
		respawnJobDetailArgs("%3", "/usr/bin/agentctl", "pl", "only"))
}
