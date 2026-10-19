package stateclassifier

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyhost"
	"github.com/srjn45/warden/internal/ptyruntime"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// fakeStream satisfies Stream for tests.
type fakeStream struct {
	id     string
	epoch  uint64
	screen string
	emits  []ptyevent.Event
	mu     sync.Mutex
}

func newFakeStream(id string, epoch uint64) *fakeStream {
	return &fakeStream{id: id, epoch: epoch}
}

func (f *fakeStream) ID() string                                         { return f.id }
func (f *fakeStream) Epoch() uint64                                      { return f.epoch }
func (f *fakeStream) Journal() *ptyruntime.Journal                       { return nil }
func (f *fakeStream) Subscribe(ptyevent.Cursor) *ptyruntime.Subscription { return nil }

func (f *fakeStream) Screen() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.screen
}

func (f *fakeStream) SetScreen(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.screen = s
}

func (f *fakeStream) Emit(ev ptyevent.Event) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emits = append(f.emits, ev)
	return uint64(len(f.emits)), nil
}

func (f *fakeStream) Emits() []ptyevent.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ptyevent.Event, len(f.emits))
	copy(out, f.emits)
	return out
}

// fakeManager satisfies Manager.
type fakeManager struct {
	mode          ptyhost.Mode
	targets       map[string]ptyhost.Target
	authoritative map[string]bool
	observers     []ptyhost.Observer
	mu            sync.Mutex
}

func newFakeManager(mode ptyhost.Mode) *fakeManager {
	return &fakeManager{
		mode:          mode,
		targets:       make(map[string]ptyhost.Target),
		authoritative: make(map[string]bool),
	}
}

func (m *fakeManager) Mode() ptyhost.Mode { return m.mode }

func (m *fakeManager) Lookup(name string) (ptyhost.Target, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.targets[name]
	return t, ok
}

func (m *fakeManager) SetTarget(name string, t ptyhost.Target, auth bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.targets[name] = t
	m.authoritative[name] = auth
}

func (m *fakeManager) Observe(o ptyhost.Observer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.observers = append(m.observers, o)
}

func (m *fakeManager) IsAuthoritative(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mode == ptyhost.ModeOn && m.authoritative[name]
}

// fakeStore satisfies Store.
type fakeStore struct {
	agents  map[string]*agentstore.Agent
	updates map[string]store.Status
	mu      sync.Mutex
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		agents:  make(map[string]*agentstore.Agent),
		updates: make(map[string]store.Status),
	}
}

func (s *fakeStore) Get(ctx context.Context, id string) (*agentstore.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.agents[id]; ok {
		return a, nil
	}
	return nil, store.ErrNotFound
}

func (s *fakeStore) UpdateStatusIf(ctx context.Context, id string, expected, next store.Status) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agents[id]
	if !ok {
		return false, store.ErrNotFound
	}
	if a.Status != expected {
		return false, nil
	}
	a.Status = next
	s.updates[id] = next
	return true, nil
}

// fakeBackend implements Backend for tests.
type mockBackend struct {
	id       string
	state    agentbackend.State
	approval *agentbackend.Approval
	limited  bool
	reason   ptyevent.WaitingReason
}

func (m *mockBackend) ID() string          { return m.id }
func (m *mockBackend) DisplayName() string { return m.id }
func (m *mockBackend) Binary() string      { return m.id }
func (m *mockBackend) InstallHint() string { return "" }
func (m *mockBackend) LaunchCmd(agentbackend.LaunchOpts) string {
	return m.id
}
func (m *mockBackend) ResumeCmd(agentbackend.ResumeOpts) (string, bool) {
	return "", false
}
func (m *mockBackend) LaunchPromptArg(string) string                { return "" }
func (m *mockBackend) HeadlessCmd(string) ([]string, bool)          { return nil, false }
func (m *mockBackend) TranscriptPath(_, _, _ string) (string, bool) { return "", false }
func (m *mockBackend) ParseTranscript(io.Reader) ([]agentbackend.Turn, error) {
	return nil, nil
}
func (m *mockBackend) DetectState(string) agentbackend.State { return m.state }
func (m *mockBackend) ParseApproval(string) (*agentbackend.Approval, bool) {
	if m.approval == nil {
		return nil, false
	}
	return m.approval, true
}
func (m *mockBackend) SystemPromptFlag(string) (string, bool) { return "", false }
func (m *mockBackend) Pricing() (agentbackend.PricingTable, bool) {
	return agentbackend.PricingTable{}, false
}
func (m *mockBackend) Capabilities() agentbackend.Caps { return agentbackend.Caps{} }
func (m *mockBackend) DetectRateLimit(string) (bool, time.Time, bool) {
	return m.limited, time.Time{}, m.limited
}

// mockProcs implements ProcessInspector.
type mockProcs struct {
	info ProcessInfo
	err  error
}

func (m *mockProcs) Inspect(ctx context.Context, rootPID int) (ProcessInfo, error) {
	return m.info, m.err
}

func TestEventDrivenActivityResetsQuiescence(t *testing.T) {
	mgr := newFakeManager(ptyhost.ModeOn)
	st := newFakeStore()
	st.agents["sess-1"] = &agentstore.Agent{ID: "sess-1", TmuxSession: "sess-1", Status: store.StatusIdle}

	stream := newFakeStream("sess-1", 1)
	mgr.SetTarget("sess-1", ptyhost.Target{Name: "sess-1", Stream: stream}, true)

	b := &mockBackend{id: "mock", state: agentbackend.StateWorking}
	resolver := func(s *agentstore.Agent) agentbackend.Backend { return b }

	cfg := DefaultConfig()
	cfg.QuiescenceGrace = 50 * time.Millisecond
	c := New(cfg, mgr, st, resolver, &mockProcs{info: ProcessInfo{RootAlive: true}}, nil)
	defer c.Close()

	now := time.Now()
	// 1. Human writes input
	c.OnPTYEvent("sess-1", ptyevent.Event{
		Kind: ptyevent.KindInput,
		Wall: now,
		Seq:  1,
		Input: &ptyevent.Input{
			Origin: ptyevent.OriginHuman,
		},
	})

	// 2. Output arrives immediately afterwards
	c.OnPTYEvent("sess-1", ptyevent.Event{
		Kind: ptyevent.KindOutput,
		Wall: now.Add(10 * time.Millisecond),
		Seq:  2,
		Output: &ptyevent.Output{
			Data: []byte("processing turn..."),
		},
	})

	// State machine proposal for EvOutputActivity + EvInput is StateWorking (dwell=0, minHits=1)
	snap, ok := c.CurrentState("sess-1")
	require.True(t, ok)
	require.Equal(t, ptyevent.StateWorking, snap.State, "output after input must transition immediately to working")

	// Store status updated in mode on
	require.Equal(t, store.StatusWorking, st.agents["sess-1"].Status)

	// Stream has KindState emitted
	emits := stream.Emits()
	require.NotEmpty(t, emits)
	require.Equal(t, ptyevent.KindState, emits[0].Kind)
	require.Equal(t, ptyevent.StateWorking, emits[0].State.To)
	require.Contains(t, emits[0].State.Evidence, ptyevent.EvOutputActivity)
}

func TestQuiescenceInvestigationNeverFromSilenceAlone(t *testing.T) {
	mgr := newFakeManager(ptyhost.ModeOn)
	st := newFakeStore()
	st.agents["sess-1"] = &agentstore.Agent{ID: "sess-1", TmuxSession: "sess-1", Status: store.StatusWorking}

	stream := newFakeStream("sess-1", 1)
	// Screen is empty / uninformative; backend reports StateUnknown
	stream.SetScreen("")
	mgr.SetTarget("sess-1", ptyhost.Target{Name: "sess-1", Stream: stream}, true)

	b := &mockBackend{id: "mock", state: agentbackend.StateUnknown}
	resolver := func(s *agentstore.Agent) agentbackend.Backend { return b }

	cfg := DefaultConfig()
	cfg.QuiescenceGrace = 20 * time.Millisecond
	c := New(cfg, mgr, st, resolver, &mockProcs{info: ProcessInfo{RootAlive: true}}, nil)
	defer c.Close()

	// Initial working state
	s := c.getOrCreate("sess-1")
	s.snapshot.State = ptyevent.StateWorking

	// Trigger investigation with silence
	c.Investigate(context.Background(), "sess-1")

	// Silence alone must NEVER change state
	snap, ok := c.CurrentState("sess-1")
	require.True(t, ok)
	require.Equal(t, ptyevent.StateWorking, snap.State, "silence alone must hold current state")
	require.Equal(t, store.StatusWorking, st.agents["sess-1"].Status)
}

func TestFusingEvidenceIdleMarkerAndSilence(t *testing.T) {
	mgr := newFakeManager(ptyhost.ModeOn)
	st := newFakeStore()
	st.agents["sess-1"] = &agentstore.Agent{ID: "sess-1", TmuxSession: "sess-1", Status: store.StatusWorking}

	stream := newFakeStream("sess-1", 1)
	stream.SetScreen("❯ \n")
	mgr.SetTarget("sess-1", ptyhost.Target{Name: "sess-1", Stream: stream}, true)

	b := &mockBackend{id: "mock", state: agentbackend.StateIdle}
	resolver := func(s *agentstore.Agent) agentbackend.Backend { return b }

	cfg := DefaultConfig()
	c := New(cfg, mgr, st, resolver, &mockProcs{info: ProcessInfo{RootAlive: true}}, nil)
	defer c.Close()

	sess := c.getOrCreate("sess-1")
	sess.snapshot.State = ptyevent.StateWorking
	sess.lastActivity = time.Now().Add(-6 * time.Second) // silence >= IdleQuiet (5s)

	c.Investigate(context.Background(), "sess-1")

	snap, ok := c.CurrentState("sess-1")
	require.True(t, ok)
	require.Equal(t, ptyevent.StateIdle, snap.State, "idle marker + silence >= 5s must commit immediately to idle")
	require.Equal(t, store.StatusIdle, st.agents["sess-1"].Status)

	trail := c.History("sess-1")
	require.NotEmpty(t, trail)
	require.Equal(t, ptyevent.StateWorking, trail[len(trail)-1].From)
	require.Equal(t, ptyevent.StateIdle, trail[len(trail)-1].To)
	require.Contains(t, trail[len(trail)-1].Evidence, ptyevent.EvIdleMarker)
	require.Contains(t, trail[len(trail)-1].Evidence, ptyevent.EvSilence)
}

func TestFusingEvidenceSubprocess(t *testing.T) {
	mgr := newFakeManager(ptyhost.ModeOn)
	st := newFakeStore()
	st.agents["sess-1"] = &agentstore.Agent{ID: "sess-1", TmuxSession: "sess-1", Status: store.StatusWorking}

	stream := newFakeStream("sess-1", 1)
	stream.SetScreen("") // quiet terminal
	mgr.SetTarget("sess-1", ptyhost.Target{Name: "sess-1", Stream: stream}, true)

	b := &mockBackend{id: "mock", state: agentbackend.StateUnknown}
	resolver := func(s *agentstore.Agent) agentbackend.Backend { return b }

	procs := &mockProcs{
		info: ProcessInfo{
			RootAlive:         true,
			HasActiveChildren: true,
			ChildCount:        2,
			ChildCommands:     []string{"bash", "go test"},
		},
	}

	cfg := DefaultConfig()
	// Disable dwell so candidate commits on min hits
	cfg.Hysteresis.Dwell[ptyevent.StateWaiting] = 0
	c := New(cfg, mgr, st, resolver, procs, nil)
	defer c.Close()

	sess := c.getOrCreate("sess-1")
	sess.snapshot.State = ptyevent.StateWorking
	sess.rootPID = 1234

	// First hit: proposes waiting{subprocess}
	c.Investigate(context.Background(), "sess-1")
	// Second hit: commits waiting{subprocess}
	c.Investigate(context.Background(), "sess-1")

	snap, ok := c.CurrentState("sess-1")
	require.True(t, ok)
	require.Equal(t, ptyevent.StateWaiting, snap.State)
	require.Equal(t, ptyevent.ReasonSubprocess, snap.Reason)

	// In store, running a tool subprocess maps to StatusWorking (it is not idle or waiting on user)
	require.Equal(t, store.StatusWorking, st.agents["sess-1"].Status)
}

func TestPromptRecognitionAndClearing(t *testing.T) {
	mgr := newFakeManager(ptyhost.ModeOn)
	st := newFakeStore()
	st.agents["sess-1"] = &agentstore.Agent{ID: "sess-1", TmuxSession: "sess-1", Status: store.StatusWorking}

	stream := newFakeStream("sess-1", 1)
	stream.SetScreen("Do you want to run bash command?\n1. Yes\n2. No")
	mgr.SetTarget("sess-1", ptyhost.Target{Name: "sess-1", Stream: stream}, true)

	b := &mockBackend{
		id:    "mock",
		state: agentbackend.StateNeedsInput,
		approval: &agentbackend.Approval{
			Action:         "Bash: git status",
			Question:       "Do you want to run bash command?",
			Options:        []string{"Yes", "No"},
			SelectedIdx:    1,
			AffirmativeIdx: 1,
		},
	}
	resolver := func(s *agentstore.Agent) agentbackend.Backend { return b }

	cfg := DefaultConfig()
	cfg.Hysteresis.Dwell[ptyevent.StateWaitingForPermission] = 0
	c := New(cfg, mgr, st, resolver, &mockProcs{info: ProcessInfo{RootAlive: true}}, nil)
	defer c.Close()

	sess := c.getOrCreate("sess-1")
	sess.snapshot.State = ptyevent.StateWorking

	// Hit 1: observes prompt, proposes waiting_for_permission
	c.Investigate(context.Background(), "sess-1")
	// Hit 2: commits waiting_for_permission
	c.Investigate(context.Background(), "sess-1")

	snap, ok := c.CurrentState("sess-1")
	require.True(t, ok)
	require.Equal(t, ptyevent.StateWaitingForPermission, snap.State)
	require.Equal(t, store.StatusWaitingForInput, st.agents["sess-1"].Status)

	// Stream received PromptObserved
	emits := stream.Emits()
	var promptEv *ptyevent.PromptEvent
	for _, e := range emits {
		if e.Kind == ptyevent.KindPrompt {
			promptEv = e.Prompt
			break
		}
	}
	require.NotNil(t, promptEv)
	require.Equal(t, ptyevent.PromptObserved, promptEv.Phase)
	require.NoError(t, promptEv.Prompt.Validate())
	require.True(t, ptyevent.Confident(promptEv.Prompt, false))

	// Prompt clears, output resumes
	b.state = agentbackend.StateWorking
	b.approval = nil
	stream.SetScreen("output continues...")

	c.Investigate(context.Background(), "sess-1")

	// Stream received PromptCleared
	emits = stream.Emits()
	cleared := false
	for _, e := range emits {
		if e.Kind == ptyevent.KindPrompt && e.Prompt.Phase == ptyevent.PromptCleared {
			cleared = true
			break
		}
	}
	require.True(t, cleared, "cleared prompt must be emitted")
}

func TestDecisionPromptRecognition(t *testing.T) {
	mgr := newFakeManager(ptyhost.ModeOn)
	st := newFakeStore()
	st.agents["sess-1"] = &agentstore.Agent{ID: "sess-1", TmuxSession: "sess-1", Status: store.StatusWorking}

	stream := newFakeStream("sess-1", 1)
	stream.SetScreen("Which plan would you like to execute?\n1. Plan A\n2. Plan B\n3. Type something else")
	mgr.SetTarget("sess-1", ptyhost.Target{Name: "sess-1", Stream: stream}, true)

	// Strategic choice question with no tool action
	b := &mockBackend{
		id:    "mock",
		state: agentbackend.StateNeedsInput,
		approval: &agentbackend.Approval{
			Question:    "Which plan would you like to execute?",
			Options:     []string{"Plan A", "Plan B", "Type something else"},
			SelectedIdx: 1,
		},
	}
	resolver := func(s *agentstore.Agent) agentbackend.Backend { return b }

	cfg := DefaultConfig()
	cfg.Hysteresis.Dwell[ptyevent.StateWaitingForDecision] = 0
	c := New(cfg, mgr, st, resolver, &mockProcs{info: ProcessInfo{RootAlive: true}}, nil)
	defer c.Close()

	sess := c.getOrCreate("sess-1")
	sess.snapshot.State = ptyevent.StateWorking

	c.Investigate(context.Background(), "sess-1")
	c.Investigate(context.Background(), "sess-1")

	snap, ok := c.CurrentState("sess-1")
	require.True(t, ok)
	require.Equal(t, ptyevent.StateWaitingForDecision, snap.State)
	require.Equal(t, store.StatusWaitingForInput, st.agents["sess-1"].Status)
}

func TestShadowModeLogsDisagreementsAndWritesNothing(t *testing.T) {
	// ModeShadow: mirror only, poller remains authoritative for store
	mgr := newFakeManager(ptyhost.ModeShadow)
	st := newFakeStore()
	// Poller currently holds agent at working
	st.agents["sess-1"] = &agentstore.Agent{ID: "sess-1", TmuxSession: "sess-1", Status: store.StatusWorking}

	stream := newFakeStream("sess-1", 1)
	stream.SetScreen("❯ \n")
	mgr.SetTarget("sess-1", ptyhost.Target{Name: "sess-1", Stream: stream}, false)

	b := &mockBackend{id: "mock", state: agentbackend.StateIdle}
	resolver := func(s *agentstore.Agent) agentbackend.Backend { return b }

	disagreements := 0
	c := New(DefaultConfig(), mgr, st, resolver, &mockProcs{info: ProcessInfo{RootAlive: true}}, nil)
	c.OnDisagreement = func(name string, cState ptyevent.State, pStatus store.Status) {
		disagreements++
	}
	defer c.Close()

	sess := c.getOrCreate("sess-1")
	sess.snapshot.State = ptyevent.StateWorking
	sess.lastActivity = time.Now().Add(-6 * time.Second)

	// Step into idle
	c.Investigate(context.Background(), "sess-1")

	snap, ok := c.CurrentState("sess-1")
	require.True(t, ok)
	require.Equal(t, ptyevent.StateIdle, snap.State)

	// Store status was NOT changed in shadow mode!
	require.Equal(t, store.StatusWorking, st.agents["sess-1"].Status, "shadow mode must write nothing to store")
	require.Equal(t, 1, disagreements, "shadow mode must report disagreement")
}

func TestRateLimitDetection(t *testing.T) {
	mgr := newFakeManager(ptyhost.ModeOn)
	st := newFakeStore()
	st.agents["sess-1"] = &agentstore.Agent{ID: "sess-1", TmuxSession: "sess-1", Status: store.StatusWorking}

	stream := newFakeStream("sess-1", 1)
	stream.SetScreen("You have hit your rate limit. Resets at 6:00am.")
	mgr.SetTarget("sess-1", ptyhost.Target{Name: "sess-1", Stream: stream}, true)

	b := &mockBackend{
		id:      "mock",
		state:   agentbackend.StateUnknown,
		limited: true,
		reason:  ptyevent.ReasonRateLimit,
	}
	resolver := func(s *agentstore.Agent) agentbackend.Backend { return b }

	cfg := DefaultConfig()
	cfg.Hysteresis.Dwell[ptyevent.StateWaiting] = 0
	c := New(cfg, mgr, st, resolver, &mockProcs{info: ProcessInfo{RootAlive: true}}, nil)
	defer c.Close()

	sess := c.getOrCreate("sess-1")
	sess.snapshot.State = ptyevent.StateWorking

	c.Investigate(context.Background(), "sess-1")
	c.Investigate(context.Background(), "sess-1")

	snap, ok := c.CurrentState("sess-1")
	require.True(t, ok)
	require.Equal(t, ptyevent.StateWaiting, snap.State)
	require.Equal(t, ptyevent.ReasonRateLimit, snap.Reason)
	require.Equal(t, store.StatusRateLimited, st.agents["sess-1"].Status)
}

func TestLifecycleProcessSpawnAndExit(t *testing.T) {
	mgr := newFakeManager(ptyhost.ModeOn)
	st := newFakeStore()
	st.agents["sess-1"] = &agentstore.Agent{ID: "sess-1", TmuxSession: "sess-1", Status: store.StatusSpawning}

	stream := newFakeStream("sess-1", 1)
	mgr.SetTarget("sess-1", ptyhost.Target{Name: "sess-1", Stream: stream}, true)

	b := &mockBackend{id: "mock"}
	resolver := func(s *agentstore.Agent) agentbackend.Backend { return b }

	c := New(DefaultConfig(), mgr, st, resolver, &mockProcs{info: ProcessInfo{RootAlive: true}}, nil)
	defer c.Close()

	// 1. ProcessSpawn
	c.OnPTYEvent("sess-1", ptyevent.Event{
		Kind: ptyevent.KindLifecycle,
		Lifecycle: &ptyevent.Lifecycle{
			Phase: ptyevent.PhaseSpawned,
			PID:   42,
		},
	})
	snap, ok := c.CurrentState("sess-1")
	require.True(t, ok)
	require.Equal(t, ptyevent.StateWaiting, snap.State)
	require.Equal(t, ptyevent.ReasonStartup, snap.Reason)
	require.Equal(t, store.StatusSpawning, st.agents["sess-1"].Status)

	// 2. Ready marker seen
	c.OnPTYEvent("sess-1", ptyevent.Event{
		Kind: ptyevent.KindLifecycle,
		Lifecycle: &ptyevent.Lifecycle{
			Phase: ptyevent.PhaseReady,
		},
	})
	snap, ok = c.CurrentState("sess-1")
	require.True(t, ok)
	require.Equal(t, ptyevent.StateIdle, snap.State)
	require.Equal(t, store.StatusIdle, st.agents["sess-1"].Status)

	// 3. ProcessExit
	c.OnPTYEvent("sess-1", ptyevent.Event{
		Kind: ptyevent.KindLifecycle,
		Lifecycle: &ptyevent.Lifecycle{
			Phase: ptyevent.PhaseExited,
		},
	})
	snap, ok = c.CurrentState("sess-1")
	require.True(t, ok)
	require.Equal(t, ptyevent.StateExited, snap.State)
	require.Equal(t, store.StatusDone, st.agents["sess-1"].Status)
}

func TestApprovalToPromptTrust(t *testing.T) {
	app := &agentbackend.Approval{
		Kind:              agentbackend.ApprovalKindTrust,
		Question:          "Do you trust this workspace?",
		Options:           []string{"Trust workspace and continue", "Don't trust and exit"},
		SelectedIdx:       1,
		AffirmativeIdx:    1,
		AffirmativeSticky: true,
	}

	prompt, err := ApprovalToPrompt(app, "antigravity", "Do you trust this workspace?", true)
	require.NoError(t, err)
	require.NotNil(t, prompt)
	require.NoError(t, prompt.Validate())
	require.Equal(t, ptyevent.ClassPermission, prompt.Class)
	require.True(t, prompt.HasCap(ptyevent.CapTrust))
	require.Equal(t, "workspace", prompt.Subject)
	require.Equal(t, ptyevent.SemAllowAlways, prompt.Options[0].Semantic)
	require.Equal(t, ptyevent.ScopeWorkspace, prompt.Options[0].Scope)
	require.True(t, prompt.Options[0].Default)
	require.Equal(t, ptyevent.SemDeny, prompt.Options[1].Semantic)
}

func TestProcessHierarchyParsing(t *testing.T) {
	raw := `  PID  PPID COMMAND
    1     0 init
  100     1 claude
  101   100 bash
  102   101 go test ./...
`
	children, comms := parsePSHierarchy(raw)
	require.Equal(t, []int{101}, children[100])
	require.Equal(t, []int{102}, children[101])
	require.Equal(t, "claude", comms[100])
	require.Equal(t, "bash", comms[101])
	require.Equal(t, "go test ./...", comms[102])
}
