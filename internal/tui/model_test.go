package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srajanpathak/agentctl/internal/approval"
	"github.com/srajanpathak/agentctl/internal/client"
	"github.com/srajanpathak/agentctl/internal/digest"
	"github.com/srajanpathak/agentctl/internal/pipeline"
	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

// fakeAPI is a test double for the tui api interface.
type fakeAPI struct {
	sessions    []*store.Session
	listErr     error
	output      string
	spawned     *client.SpawnParams
	terminated  string
	termErr     error
	deleted     string
	deleteErr   error
	sentTo      string
	sentText    string
	dirListing  client.DirListing
	dirListErr  error
	approvals   []approval.View
	approvalsOn bool
	approveErr  error
	approvedID  string
	approvedOpt int
	approvedFP  string
	pipelines   []*pipeline.Pipeline
	retried     string // "<pid>/<job>" of the last PipelineRetry
	canceled    string // pid of the last PipelineCancel
	deletedPipe string // pid of the last PipelineDelete
	deletePErr  error  // error PipelineDelete returns (e.g. simulating a 409)
	digest      *digest.Digest
	digestErr   error
	pressure    client.PressureStatus
	pressureErr error
}

func (f *fakeAPI) List(context.Context) ([]*store.Session, error) { return f.sessions, f.listErr }
func (f *fakeAPI) Output(_ context.Context, _ string, _ int) (string, error) {
	return f.output, nil
}
func (f *fakeAPI) Spawn(_ context.Context, p client.SpawnParams) (*store.Session, error) {
	f.spawned = &p
	return &store.Session{ID: "agent-new"}, nil
}
func (f *fakeAPI) Terminate(_ context.Context, id string) error {
	f.terminated = id
	return f.termErr
}
func (f *fakeAPI) Delete(_ context.Context, id string, _ bool) error {
	f.deleted = id
	return f.deleteErr
}
func (f *fakeAPI) Input(_ context.Context, id, text string) error {
	f.sentTo, f.sentText = id, text
	return nil
}
func (f *fakeAPI) ListDirs(_ context.Context, _ string) (client.DirListing, error) {
	return f.dirListing, f.dirListErr
}
func (f *fakeAPI) Approvals(_ context.Context) (bool, []approval.View, error) {
	return f.approvalsOn, f.approvals, nil
}
func (f *fakeAPI) Approve(_ context.Context, id string, option int, fp string) error {
	f.approvedID, f.approvedOpt, f.approvedFP = id, option, fp
	return f.approveErr
}
func (f *fakeAPI) PipelineList(context.Context) ([]*pipeline.Pipeline, error) {
	return f.pipelines, nil
}
func (f *fakeAPI) PipelineGet(_ context.Context, id string) (*pipeline.Pipeline, error) {
	for _, p := range f.pipelines {
		if p.ID == id {
			return p, nil
		}
	}
	return nil, fmt.Errorf("pipeline %q not found", id)
}
func (f *fakeAPI) PipelineRetry(_ context.Context, pid, job string) error {
	f.retried = pid + "/" + job
	return nil
}
func (f *fakeAPI) PipelineCancel(_ context.Context, pid string) error {
	f.canceled = pid
	return nil
}
func (f *fakeAPI) PipelineDelete(_ context.Context, pid string) error {
	if f.deletePErr != nil {
		return f.deletePErr
	}
	f.deletedPipe = pid
	return nil
}
func (f *fakeAPI) Digest(_ context.Context, _ string) (*digest.Digest, error) {
	return f.digest, f.digestErr
}
func (f *fakeAPI) Pressure(context.Context) (client.PressureStatus, error) {
	return f.pressure, f.pressureErr
}

// step applies a msg and returns the updated concrete Model.
func step(m Model, msg tea.Msg) Model {
	nm, _ := m.Update(msg)
	return nm.(Model)
}

func key(s string) tea.KeyMsg {
	switch s {
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "\t":
		return tea.KeyMsg{Type: tea.KeyTab}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func threeSessions() []*store.Session {
	return []*store.Session{
		{ID: "a", Status: store.StatusWorking},
		{ID: "b", Status: store.StatusIdle},
		{ID: "c", Status: store.StatusWaitingForInput},
	}
}

func TestCursorMovesAndClamps(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, sessionsMsg{sessions: threeSessions()})
	require.Equal(t, 0, m.cursor)
	m = step(m, key("down"))
	m = step(m, key("down"))
	require.Equal(t, 2, m.cursor)
	m = step(m, key("down")) // clamp at last
	require.Equal(t, 2, m.cursor)
	m = step(m, key("up"))
	require.Equal(t, 1, m.cursor)
	require.Equal(t, "b", m.selectedID())
}

func TestSessionsMsgRepinsByID(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, sessionsMsg{sessions: threeSessions()})
	m = step(m, key("down")) // cursor=1 → "b"
	// New snapshot reorders: b is now first. Selection should follow id "b".
	m = step(m, sessionsMsg{sessions: []*store.Session{
		{ID: "b", Status: store.StatusIdle},
		{ID: "a", Status: store.StatusWorking},
	}})
	require.Equal(t, "b", m.selectedID())
}

func TestListErrorSetsDisconnected(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, sessionsMsg{err: client.ErrDaemonDown})
	require.False(t, m.connected)
}

func TestQuitKey(t *testing.T) {
	m := New(&fakeAPI{})
	_, cmd := m.Update(key("q"))
	require.NotNil(t, cmd, "q should return a command (tea.Quit)")
}

func TestViewDoesNotPanic(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, sessionsMsg{sessions: threeSessions()})
	require.NotEmpty(t, m.View())
}

func TestOutputMsgFillsSelected(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, sessionsMsg{sessions: threeSessions()}) // cursor=0 → "a"
	m = step(m, outputMsg{id: "a", text: "hello output"})
	require.Equal(t, "hello output", m.output)
}

func TestOutputMsgIgnoresStaleID(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, sessionsMsg{sessions: threeSessions()}) // selected "a"
	m = step(m, outputMsg{id: "a", text: "for a"})
	m = step(m, outputMsg{id: "c", text: "stale"}) // not selected
	require.Equal(t, "for a", m.output)
}

func TestOutputMsgErrorKeepsPriorContent(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, sessionsMsg{sessions: threeSessions()}) // selected "a"
	m = step(m, outputMsg{id: "a", text: "live output"})
	require.Equal(t, "live output", m.output)
	// A transient fetch error (or a poll-loop timeout) must not blank the pane
	// the user is reading.
	m = step(m, outputMsg{id: "a", err: fmt.Errorf("capture failed")})
	require.Equal(t, "live output", m.output, "a fetch error must keep the last good output")
}

func TestTabFocusesOutput(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, sessionsMsg{sessions: threeSessions()})
	require.False(t, m.outputFocused)
	m = step(m, key("\t")) // tab
	require.True(t, m.outputFocused)
}

func TestNewAgentModeFlow(t *testing.T) {
	f := &fakeAPI{}
	m := New(f)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, key("n"))
	require.Equal(t, modeNewAgent, m.mode)
	// type a prompt
	m = step(m, key("research SSE"))
	// submit with ctrl+s
	m, _ = submit(m, tea.KeyMsg{Type: tea.KeyCtrlS})
	require.NotNil(t, f.spawned)
	require.Equal(t, "research SSE", f.spawned.Prompt)
}

func TestNewAgentEmptyPromptSpawnsInteractive(t *testing.T) {
	f := &fakeAPI{}
	m := New(f)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, key("n"))
	require.Equal(t, modeNewAgent, m.mode)
	// Submit immediately, without typing a prompt.
	m, _ = submit(m, tea.KeyMsg{Type: tea.KeyCtrlS})
	require.NotNil(t, f.spawned, "empty prompt now spawns an interactive agent")
	require.Equal(t, "", f.spawned.Prompt)
	require.NotEqual(t, "prompt was empty", m.status)
}

func TestNewAgentEscCancels(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, key("n"))
	m = step(m, key("esc"))
	require.Equal(t, modeNormal, m.mode)
}

func TestSpawnDoneSelectsNewAgent(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, sessionsMsg{sessions: []*store.Session{{ID: "agent-new"}, {ID: "x"}}})
	m = step(m, spawnDoneMsg{id: "agent-new"})
	require.Equal(t, "agent-new", m.pendingSelect)
	// next list refresh pins it
	m = step(m, sessionsMsg{sessions: []*store.Session{{ID: "x"}, {ID: "agent-new"}}})
	require.Equal(t, "agent-new", m.selectedID())
}

func TestSessionsMsgGroupsBySourceDir(t *testing.T) {
	now := time.Now()
	m := step(New(&fakeAPI{}), sessionsMsg{sessions: []*store.Session{
		{ID: "b1", Workdir: "/b", UpdatedAt: now.Add(-1 * time.Minute)},
		{ID: "a1", Workdir: "/a", UpdatedAt: now.Add(-2 * time.Minute)},
		{ID: "b2", Workdir: "/b", UpdatedAt: now.Add(-3 * time.Minute)},
	}})
	ids := []string{m.sessions[0].ID, m.sessions[1].ID, m.sessions[2].ID}
	require.Equal(t, []string{"b1", "b2", "a1"}, ids, "classic model stores grouped order")
}

// submit is a test helper: applies one key, runs the resulting command (so
// fake-api side effects like Spawn are recorded), and returns model + cmd.
func submit(m Model, msg tea.Msg) (Model, tea.Cmd) {
	nm, cmd := m.Update(msg)
	if cmd != nil {
		cmd()
	}
	return nm.(Model), cmd
}

func TestSendMessageFlow(t *testing.T) {
	f := &fakeAPI{}
	m := New(f)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, sessionsMsg{sessions: threeSessions()}) // selected "a"
	m = step(m, key("s"))
	require.Equal(t, modeSendMsg, m.mode)
	m = step(m, key("hello"))
	m, _ = submit(m, key("enter"))
	require.Equal(t, "a", f.sentTo)
	require.Equal(t, "hello", f.sentText)
	require.Equal(t, modeNormal, m.mode)
}

func TestKillConfirmTerminatesAndDeletes(t *testing.T) {
	f := &fakeAPI{}
	m := New(f)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, sessionsMsg{sessions: threeSessions()}) // selected "a"
	m = step(m, key("x"))
	require.Equal(t, modeConfirmKill, m.mode)
	m, _ = submit(m, key("y")) // confirm → kill & remove
	require.Equal(t, "a", f.terminated)
	require.Equal(t, "a", f.deleted, "kill also removes the record from the list")
}

// An already-dead agent (terminate errors) must still be removed: the terminate
// step is best-effort, so the record is deleted regardless.
func TestKillRemovesEvenWhenTerminateFails(t *testing.T) {
	f := &fakeAPI{termErr: context.DeadlineExceeded}
	m := New(f)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, sessionsMsg{sessions: threeSessions()}) // selected "a"
	m = step(m, key("x"))
	m, _ = submit(m, key("y"))
	require.Equal(t, "a", f.deleted, "delete runs even though terminate failed")
}

func TestKillEscCancels(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, sessionsMsg{sessions: threeSessions()})
	m = step(m, key("x"))
	m = step(m, key("esc"))
	require.Equal(t, modeNormal, m.mode)
}

func TestHelpToggle(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, key("?"))
	require.Equal(t, modeHelp, m.mode)
	m = step(m, key("?")) // any key closes
	require.Equal(t, modeNormal, m.mode)
}

func TestAttachDoneShowsError(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, attachDoneMsg{err: context.DeadlineExceeded})
	require.Contains(t, m.status, "attach")
}

func TestAttachNoOpWhenNoSelection(t *testing.T) {
	m := New(&fakeAPI{})
	_, cmd := m.Update(key("a"))
	require.Nil(t, cmd, "attach with no selection does nothing")
}

func TestViewDoesNotOverflowHeight(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	for i := 0; i < 40; i++ {
		m.sessions = append(m.sessions, &store.Session{ID: fmt.Sprintf("a-%02d", i), Status: store.StatusWorking})
	}
	out := m.View()
	require.LessOrEqual(t, strings.Count(out, "\n")+1, 30, "View must not exceed terminal height")
}

func TestModelNewAgentResolvesTargetDir(t *testing.T) {
	now := time.Now()
	m := New(&fakeAPI{})
	m = step(m, sessionsMsg{sessions: []*store.Session{{ID: "a1", Workdir: "/work/api", UpdatedAt: now}}})
	m = step(m, key("n"))
	require.Equal(t, modeNewAgent, m.mode)
	require.Equal(t, "/work/api", m.targetDir)
}

func TestModelOpenDirAddsPlaceholder(t *testing.T) {
	m := New(&fakeAPI{dirListing: client.DirListing{Path: "/work/api"}})
	m = step(m, key("o"))
	require.Equal(t, modeOpenDir, m.mode)
	m = step(m, openDirMsg{dir: "/work/api"})
	require.Equal(t, modeNormal, m.mode)
	items := m.items()
	require.Len(t, items, 1)
	require.Nil(t, items[0].session)
	require.Equal(t, "/work/api", items[0].dir)
}

func TestModelCloseOpenedDirWithX(t *testing.T) {
	m := New(&fakeAPI{})
	m.openedDirs["/work/empty"] = time.Now()
	m = step(m, key("x"))
	require.Empty(t, m.openedDirs)
	require.NotEqual(t, modeConfirmKill, m.mode)
}

func TestApprovalsMsgPopulatesState(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, approvalsMsg{enabled: true, views: []approval.View{{ID: "a1", Recognized: true, Options: []string{"Yes", "No"}}}})
	require.True(t, m.approvalsOn)
	require.Len(t, m.approvals, 1)
	// inbox row is present at top of items() when enabled
	require.True(t, m.items()[0].approvals)
}

func TestIKeyFocusesInbox(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, approvalsMsg{enabled: true, views: []approval.View{{ID: "a1", Recognized: true, Options: []string{"Yes", "No"}}}})
	m = step(m, key("i"))
	require.True(t, m.apprFocused)
	require.Equal(t, 0, m.cursor)
}

func TestApprovalsDisableClearsFocus(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, approvalsMsg{enabled: true, views: []approval.View{{ID: "a1", Recognized: true, Options: []string{"Yes"}}}})
	m = step(m, key("i"))
	require.True(t, m.apprFocused)
	m = step(m, approvalsMsg{enabled: false})
	require.False(t, m.apprFocused)
}

func TestInboxNumberKeyAnswers(t *testing.T) {
	fa := &fakeAPI{}
	m := New(fa)
	m = step(m, approvalsMsg{enabled: true, views: []approval.View{
		{ID: "a1", Recognized: true, Options: []string{"Yes", "No"}, Fingerprint: "ff"},
	}})
	m = step(m, key("i")) // focus the inbox
	require.True(t, m.apprFocused)
	// pressing "1" returns an approveCmd; run it to drive the fake api.
	_, cmd := m.Update(key("1"))
	require.NotNil(t, cmd)
	cmd() // executes approveCmd → fakeAPI.Approve records the call
	require.Equal(t, "a1", fa.approvedID)
	require.Equal(t, 1, fa.approvedOpt)
	require.Equal(t, "ff", fa.approvedFP)
}

func TestApprovalsPassiveDoesNotMoveCursor(t *testing.T) {
	fa := &fakeAPI{}
	m := New(fa)
	// two agents so there is a non-inbox row to sit on
	m = step(m, sessionsMsg{sessions: []*store.Session{{ID: "a1"}, {ID: "a2"}}})
	m = step(m, approvalsMsg{enabled: true, views: nil}) // inbox row now at index 0
	// move cursor onto a real agent row (index 1+)
	m = step(m, key("j"))
	before := m.cursor
	require.Greater(t, before, 0)
	// a new approval arrives
	m = step(m, approvalsMsg{enabled: true, views: []approval.View{{ID: "a1", Recognized: true, Options: []string{"Yes"}}}})
	require.Equal(t, before, m.cursor)          // selection did not move
	require.Equal(t, 1, m.items()[0].apprCount) // inbox row count bumped
}

func TestApprovalsToggleOffHidesRow(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, approvalsMsg{enabled: true, views: []approval.View{{ID: "a1", Recognized: true, Options: []string{"Yes"}}}})
	require.True(t, m.items()[0].approvals)
	m = step(m, approvalsMsg{enabled: false})
	for _, it := range m.items() {
		require.False(t, it.approvals)
	}
}

func TestPipelinesMsgStoresPipelines(t *testing.T) {
	m := New(&fakeAPI{})
	updated, _ := m.Update(pipelinesMsg{pipelines: []*pipeline.Pipeline{
		{ID: "demo", Name: "demo", Status: pipeline.StatusRunning, Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobRunning}}},
	}})
	if got := updated.(Model).pipelines; len(got) != 1 || got[0].ID != "demo" {
		t.Fatalf("pipelines not stored: %+v", got)
	}
}

func TestPipelineActionMsgRefetches(t *testing.T) {
	m := New(&fakeAPI{})
	_, cmd := m.Update(pipelineActionMsg{err: nil})
	if cmd == nil {
		t.Fatalf("a pipeline action should trigger a refetch cmd")
	}
}

// donePipeline returns a stopped pipeline (no live jobs) selectable at cursor 0.
func donePipeline() []*pipeline.Pipeline {
	return []*pipeline.Pipeline{
		{ID: "demo", Name: "demo", Status: pipeline.StatusCanceled, Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobDone}}},
	}
}

func TestPipelineDeleteKeyConfirmsThenDeletes(t *testing.T) {
	f := &fakeAPI{}
	m := New(f)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, pipelinesMsg{pipelines: donePipeline()})
	require.NotNil(t, itemAt(m.items(), m.cursor).pipeline, "pipeline header should be selected at cursor 0")
	m = step(m, key("D"))
	require.Equal(t, modeConfirmDeletePipeline, m.mode)
	m, _ = submit(m, key("y")) // confirm
	require.Equal(t, "demo", f.deletedPipe)
	require.Equal(t, modeNormal, m.mode)
}

func TestPipelineDeleteRefusedWhenLiveJobs(t *testing.T) {
	f := &fakeAPI{}
	m := New(f)
	m = step(m, pipelinesMsg{pipelines: []*pipeline.Pipeline{
		{ID: "demo", Status: pipeline.StatusRunning, Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobRunning}}},
	}})
	m = step(m, key("D"))
	require.NotEqual(t, modeConfirmDeletePipeline, m.mode, "must not enter confirm while a job is live")
	require.Empty(t, f.deletedPipe)
	require.Contains(t, m.status, "cancel")
}

func TestPipelineDeleteEscCancels(t *testing.T) {
	f := &fakeAPI{}
	m := New(f)
	m = step(m, pipelinesMsg{pipelines: donePipeline()})
	m = step(m, key("D"))
	require.Equal(t, modeConfirmDeletePipeline, m.mode)
	m = step(m, key("esc"))
	require.Equal(t, modeNormal, m.mode)
	require.Empty(t, f.deletedPipe)
}

// A non-pipeline selection (an agent) must not trigger pipeline delete.
func TestPipelineDeleteIgnoredOnAgentRow(t *testing.T) {
	f := &fakeAPI{}
	m := New(f)
	m = step(m, sessionsMsg{sessions: threeSessions()}) // selected agent "a"
	m = step(m, key("D"))
	require.NotEqual(t, modeConfirmDeletePipeline, m.mode)
	require.Empty(t, f.deletedPipe)
}
