package tui

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
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
	renamedID   string // id of the last SetName
	renamedName string // name of the last SetName
	renameErr   error  // error SetName returns

	autoApproveID   string // id of the last SetAutoApprove
	autoApproveVal  bool   // enabled value of the last SetAutoApprove
	autoApproveErr  error  // error SetAutoApprove returns
	forceCompactID  string // id of the last SetForceCompact
	forceCompactSt  string // state of the last SetForceCompact
	forceCompactErr error  // error SetForceCompact returns
	dirListing      client.DirListing
	dirListErr      error
	clonedURL       string // url of the last CloneRepo
	clonedDir       string // dir CloneRepo returns
	cloneErr        error  // error CloneRepo returns
	approvals       []approval.View
	approvalsOn     bool
	approveErr      error
	approvedID      string
	approvedOpt     int
	approvedFP      string
	pipelines       []*pipeline.Pipeline
	retried         string // "<pid>/<job>" of the last PipelineRetry
	paused          string // pid of the last PipelinePause
	resumed         string // pid of the last PipelineResume
	canceled        string // pid of the last PipelineCancel
	deletedPipe     string // pid of the last PipelineDelete
	deletePErr      error  // error PipelineDelete returns (e.g. simulating a 409)
	digest          *digest.Digest
	digestErr       error
	pressure        client.PressureStatus
	pressureErr     error
	ctxEntries      []client.ContextEntry
	ctxListErr      error
	messages        []client.Message
	msgErr          error
	msgLimit        int // last limit passed to MsgRecent

	// backend registry (Backends page)
	backends     client.BackendsState
	backendsErr  error // error ListBackends/RescanBackends return
	rescanned    bool  // RescanBackends was called
	tieredID     string
	tieredTier   string
	tierErr      error
	enabledID    string
	enabledVal   bool
	enabledErr   error
	defaultedID  string
	defaultErr   error
	thinkingMode string // last mode passed to SetThinkingMode
	thinkingErr  error

	// projects (Phase 4 tree nesting)
	projects        []projectstore.Project
	projectsErr     error
	closedProjectID string // id of the last CloseProject
	closeProjectErr error
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
func (f *fakeAPI) SetName(_ context.Context, id, name string) error {
	f.renamedID, f.renamedName = id, name
	return f.renameErr
}
func (f *fakeAPI) SetAutoApprove(_ context.Context, id string, enabled bool) error {
	f.autoApproveID, f.autoApproveVal = id, enabled
	return f.autoApproveErr
}
func (f *fakeAPI) SetForceCompact(_ context.Context, id, state string) error {
	f.forceCompactID, f.forceCompactSt = id, state
	return f.forceCompactErr
}
func (f *fakeAPI) ListDirs(_ context.Context, _ string) (client.DirListing, error) {
	return f.dirListing, f.dirListErr
}
func (f *fakeAPI) CloneRepo(_ context.Context, url string) (string, error) {
	f.clonedURL = url
	return f.clonedDir, f.cloneErr
}
func (f *fakeAPI) ListProjects(context.Context) ([]projectstore.Project, error) {
	return f.projects, f.projectsErr
}
func (f *fakeAPI) CloseProject(_ context.Context, id string) (projectstore.Project, error) {
	f.closedProjectID = id
	return projectstore.Project{ID: id, Status: projectstore.StatusClosed}, f.closeProjectErr
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
func (f *fakeAPI) PipelinePause(_ context.Context, pid string) error {
	f.paused = pid
	return nil
}
func (f *fakeAPI) PipelineResume(_ context.Context, pid string) error {
	f.resumed = pid
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
func (f *fakeAPI) CtxList(_ context.Context, _ string) ([]client.ContextEntry, error) {
	return f.ctxEntries, f.ctxListErr
}
func (f *fakeAPI) MsgRecent(_ context.Context, limit int) ([]client.Message, error) {
	f.msgLimit = limit
	return f.messages, f.msgErr
}
func (f *fakeAPI) GetAutopilot(context.Context) (client.AutopilotStatus, error) {
	return client.AutopilotStatus{}, nil
}
func (f *fakeAPI) SetAutopilot(_ context.Context, _ bool, _ string) (client.AutopilotStatus, error) {
	return client.AutopilotStatus{}, nil
}
func (f *fakeAPI) ListBackends(context.Context) (client.BackendsState, error) {
	return f.backends, f.backendsErr
}
func (f *fakeAPI) RescanBackends(context.Context) (client.BackendsState, error) {
	f.rescanned = true
	return f.backends, f.backendsErr
}
func (f *fakeAPI) SetBackendTier(_ context.Context, id, tier string) (client.Backend, error) {
	f.tieredID, f.tieredTier = id, tier
	return client.Backend{ID: id, Tier: tier}, f.tierErr
}
func (f *fakeAPI) SetBackendEnabled(_ context.Context, id string, enabled bool) (client.Backend, error) {
	f.enabledID, f.enabledVal = id, enabled
	return client.Backend{ID: id, Enabled: enabled}, f.enabledErr
}
func (f *fakeAPI) SetDefaultBackend(_ context.Context, id string) (client.BackendsState, error) {
	f.defaultedID = id
	return f.backends, f.defaultErr
}
func (f *fakeAPI) SetThinkingMode(_ context.Context, mode string) (client.BackendSettings, error) {
	f.thinkingMode = mode
	return client.BackendSettings{InternalThinkingMode: mode}, f.thinkingErr
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

// donePipeline returns a stopped pipeline (no live jobs) selectable at cursor 0.
func donePipeline() []*pipeline.Pipeline {
	return []*pipeline.Pipeline{
		{ID: "demo", Name: "demo", Status: pipeline.StatusCanceled, Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobDone}}},
	}
}
