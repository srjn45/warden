package orchestrator

import (
	"context"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/llm"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
)

// fakeDaemon is an in-memory Daemon for tests: it records mutating calls and
// serves canned reads. It satisfies the full Daemon interface.
type fakeDaemon struct {
	sessions  []*store.Session
	approvals []approval.View
	apprOn    bool
	inbox     []client.Message
	ctxVal    string
	ctxList   []client.ContextEntry
	pipelines []*pipeline.Pipeline

	lastSpawn      client.SpawnParams
	spawnCalls     int
	inputCalls     int
	terminateCalls int
	terminated     []string
	deleteCalls    int
	deleted        []string
	restoreCalls   int
	approveCalls   int
	commitCalls    int
	pushCalls      int
	syncCalls      int
	checkCalls     int
	ctxSetCalls    int
	msgSendCalls   int
	pipeCreate     int
	pipeCancel     int

	getErr error
}

// reads
func (f *fakeDaemon) List(context.Context) ([]*store.Session, error) { return f.sessions, nil }
func (f *fakeDaemon) Get(_ context.Context, id string) (*store.Session, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	for _, s := range f.sessions {
		if s.ID == id {
			return s, nil
		}
	}
	return nil, errNotFound(id)
}
func (f *fakeDaemon) Output(context.Context, string, int) (string, error) { return "output tail", nil }
func (f *fakeDaemon) Approvals(context.Context) (bool, []approval.View, error) {
	return f.apprOn, f.approvals, nil
}
func (f *fakeDaemon) MsgInbox(context.Context, string, bool) ([]client.Message, error) {
	return f.inbox, nil
}
func (f *fakeDaemon) CtxGet(context.Context, string) (client.ContextEntry, error) {
	return client.ContextEntry{Value: f.ctxVal}, nil
}
func (f *fakeDaemon) CtxList(context.Context, string) ([]client.ContextEntry, error) {
	return f.ctxList, nil
}
func (f *fakeDaemon) PipelineList(context.Context) ([]*pipeline.Pipeline, error) {
	return f.pipelines, nil
}
func (f *fakeDaemon) PipelineGet(_ context.Context, id string) (*pipeline.Pipeline, error) {
	for _, p := range f.pipelines {
		if p.ID == id {
			return p, nil
		}
	}
	return nil, errNotFound(id)
}
func (f *fakeDaemon) CollabConflicts(context.Context) ([]client.Conflict, error) { return nil, nil }

// mutations
func (f *fakeDaemon) Spawn(_ context.Context, p client.SpawnParams) (*store.Session, error) {
	f.spawnCalls++
	f.lastSpawn = p
	return &store.Session{ID: "new-agent", Type: store.Type(p.Type)}, nil
}
func (f *fakeDaemon) Input(context.Context, string, string) error { f.inputCalls++; return nil }
func (f *fakeDaemon) Terminate(_ context.Context, id string) error {
	f.terminateCalls++
	f.terminated = append(f.terminated, id)
	return nil
}
func (f *fakeDaemon) Delete(_ context.Context, id string, _ bool) error {
	f.deleteCalls++
	f.deleted = append(f.deleted, id)
	return nil
}
func (f *fakeDaemon) Restore(context.Context, string) error { f.restoreCalls++; return nil }
func (f *fakeDaemon) Approve(context.Context, string, int, string) error {
	f.approveCalls++
	return nil
}
func (f *fakeDaemon) GitCommit(context.Context, string, string, string) (lifecycle.CommitResult, error) {
	f.commitCalls++
	return lifecycle.CommitResult{}, nil
}
func (f *fakeDaemon) GitPush(context.Context, string, string) (lifecycle.PushResult, error) {
	f.pushCalls++
	return lifecycle.PushResult{}, nil
}
func (f *fakeDaemon) GitSync(context.Context, string, string, string) (lifecycle.SyncResult, error) {
	f.syncCalls++
	return lifecycle.SyncResult{}, nil
}
func (f *fakeDaemon) Check(context.Context, string, string, string) (lifecycle.CheckResult, error) {
	f.checkCalls++
	return lifecycle.CheckResult{}, nil
}
func (f *fakeDaemon) CtxSet(context.Context, string, string, string) (client.ContextEntry, error) {
	f.ctxSetCalls++
	return client.ContextEntry{}, nil
}
func (f *fakeDaemon) MsgSend(context.Context, string, string, string) (client.Message, bool, error) {
	f.msgSendCalls++
	return client.Message{ID: "m1"}, false, nil
}
func (f *fakeDaemon) PipelineCreate(context.Context, string) (*pipeline.Pipeline, error) {
	f.pipeCreate++
	return &pipeline.Pipeline{ID: "p1"}, nil
}
func (f *fakeDaemon) PipelineCancel(context.Context, string) error { f.pipeCancel++; return nil }

type notFoundErr string

func (e notFoundErr) Error() string { return "not found: " + string(e) }
func errNotFound(id string) error   { return notFoundErr(id) }

// --- session builders shared across tests ---

func sessionWith(id string, st store.Status) *store.Session {
	return &store.Session{ID: id, Type: store.TypeDevelopment, Status: st}
}
func active(id string) *store.Session          { return sessionWith(id, store.StatusWorking) }
func done(id string) *store.Session            { return sessionWith(id, store.StatusDone) }
func errored(id string) *store.Session         { return sessionWith(id, store.StatusErrored) }
func waitingApproval(id string) *store.Session { return sessionWith(id, store.StatusWaitingForInput) }

// --- chat / gate fakes ---

// scriptChatter returns its queued replies in order, one per Chat call.
type scriptChatter struct {
	replies []llm.Reply
	calls   int
	gotMsgs [][]llm.Message
}

func (s *scriptChatter) Chat(_ context.Context, msgs []llm.Message, _ []llm.ToolSchema) (llm.Reply, error) {
	s.gotMsgs = append(s.gotMsgs, msgs)
	r := llm.Reply{Text: "(out of script)"}
	if s.calls < len(s.replies) {
		r = s.replies[s.calls]
	}
	s.calls++
	return r, nil
}

// errChatter always fails — models the local model being down.
type errChatter struct{ err error }

func (e errChatter) Chat(context.Context, []llm.Message, []llm.ToolSchema) (llm.Reply, error) {
	return llm.Reply{}, e.err
}

// spyGate records Confirm calls and returns a canned decision.
type spyGate struct {
	decision     Decision
	confirmCalls int
	proposed     [][]ToolCall
}

func (s *spyGate) Confirm(calls []ToolCall) Decision {
	s.confirmCalls++
	s.proposed = append(s.proposed, calls)
	d := s.decision
	if d.Action == Approve && d.Calls == nil {
		d.Calls = calls // approve runs exactly what was proposed
	}
	return d
}

func (s *spyGate) proposedIDs() []string {
	var ids []string
	for _, batch := range s.proposed {
		for _, c := range batch {
			ids = append(ids, argStr(c.Args, "ticket"))
		}
	}
	return ids
}
