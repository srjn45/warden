package tui

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srajanpathak/agentctl/internal/approval"
	"github.com/srajanpathak/agentctl/internal/client"
	"github.com/srajanpathak/agentctl/internal/digest"
	"github.com/srajanpathak/agentctl/internal/pipeline"
	"github.com/srajanpathak/agentctl/internal/store"
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
	ctxEntries  []client.ContextEntry
	ctxListErr  error
	messages    []client.Message
	msgErr      error
	msgLimit    int // last limit passed to MsgRecent
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
func (f *fakeAPI) CtxList(_ context.Context, _ string) ([]client.ContextEntry, error) {
	return f.ctxEntries, f.ctxListErr
}
func (f *fakeAPI) MsgRecent(_ context.Context, limit int) ([]client.Message, error) {
	f.msgLimit = limit
	return f.messages, f.msgErr
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
