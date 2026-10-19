package promptengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentbackend/backends"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/brainconsult"
	"github.com/srjn45/warden/internal/decisionrouter"
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyhost"
	"github.com/srjn45/warden/internal/ptyruntime"
	"github.com/srjn45/warden/internal/stateclassifier"
	"github.com/srjn45/warden/internal/store"
)

const fixtures = "../agentbackend/backends/testdata/antigravity"

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// scriptStream is a journal-less Stream whose screen can follow a script: each
// Screen() call consumes the next entry (the last one repeats). That makes the
// classify → decide → re-read-before-write sequence deterministic.
type scriptStream struct {
	mu     sync.Mutex
	script []string
	calls  int
	emits  []ptyevent.Event
}

func (s *scriptStream) set(screens ...string) {
	s.mu.Lock()
	s.script, s.calls = screens, 0
	s.mu.Unlock()
}
func (s *scriptStream) ID() string                                         { return "s" }
func (s *scriptStream) Epoch() uint64                                      { return 1 }
func (s *scriptStream) Journal() *ptyruntime.Journal                       { return nil }
func (s *scriptStream) Subscribe(ptyevent.Cursor) *ptyruntime.Subscription { return nil }
func (s *scriptStream) Screen() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.script) == 0 {
		return ""
	}
	i := s.calls
	if i >= len(s.script) {
		i = len(s.script) - 1
	}
	s.calls++
	return s.script[i]
}
func (s *scriptStream) Emit(ev ptyevent.Event) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emits = append(s.emits, ev)
	return uint64(len(s.emits)), nil
}
func (s *scriptStream) decisions() []*ptyevent.DecisionEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*ptyevent.DecisionEvent
	for _, e := range s.emits {
		if e.Kind == ptyevent.KindDecision {
			out = append(out, e.Decision)
		}
	}
	return out
}

type recWriter struct {
	mu    sync.Mutex
	wrote [][]byte
}

func (w *recWriter) WriteInput(_ context.Context, d []byte, _ ptyevent.Origin, _ string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.wrote = append(w.wrote, append([]byte(nil), d...))
	return nil
}
func (w *recWriter) Lease() ptyevent.Lease { return ptyevent.Lease{} }
func (w *recWriter) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, b := range w.wrote {
		out = append(out, string(b))
	}
	return out
}

type fakeMgr struct {
	st     *scriptStream
	w      *recWriter
	auth   bool
	mu     sync.Mutex
	obs    []ptyhost.Observer
	direct bool
}

func (m *fakeMgr) Mode() ptyhost.Mode {
	if m.auth {
		return ptyhost.ModeOn
	}
	return ptyhost.ModeShadow
}
func (m *fakeMgr) Lookup(string) (ptyhost.Target, bool) {
	return ptyhost.Target{Stream: m.st, Presentation: ptyevent.PresentationTmux}, true
}
func (m *fakeMgr) ObserveAgents(o ptyhost.Observer) { m.obs = append(m.obs, o) }
func (m *fakeMgr) IsAuthoritative(string) bool      { return m.auth }
func (m *fakeMgr) Target(string) (Target, bool)     { return Target{Stream: m.st, Writer: m.w}, true }
func (m *fakeMgr) Authoritative(n string) bool      { return m.auth }

type fakeAgents struct {
	mu sync.Mutex
	a  *agentstore.Agent
}

func (f *fakeAgents) Get(context.Context, string) (*agentstore.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *f.a
	return &cp, nil
}
func (f *fakeAgents) UpdateStatusIf(_ context.Context, _ string, _, next store.Status) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.a.Status = next
	return true, nil
}

type fakeProcs struct{ info stateclassifier.ProcessInfo }

func (p fakeProcs) Inspect(context.Context, int) (stateclassifier.ProcessInfo, error) {
	return p.info, nil
}

type fakeConsult struct {
	mu    sync.Mutex
	calls int
}

func (c *fakeConsult) Consult(context.Context, brainconsult.Request) (brainconsult.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return brainconsult.Result{Answer: brainconsult.AnswerSelectOption, Option: 2, Reason: "scripted"}, nil
}
func (c *fakeConsult) n() int { c.mu.Lock(); defer c.mu.Unlock(); return c.calls }

type rig struct {
	t       *testing.T
	st      *scriptStream
	w       *recWriter
	mgr     *fakeMgr
	agent   *agentstore.Agent
	cls     *stateclassifier.Classifier
	eng     *Engine
	consult *fakeConsult
}

func newRig(t *testing.T, posture ptyevent.Posture, act bool, workdir string) *rig {
	t.Helper()
	return newRigCfg(t, posture, act, workdir, nil)
}

// newRigCfg is newRig with a hook to adjust the engine config (e.g. attach a
// decision trace and metrics) before the engine is built.
func newRigCfg(t *testing.T, posture ptyevent.Posture, act bool, workdir string, mutate func(*Config)) *rig {
	t.Helper()
	r := &rig{t: t, st: &scriptStream{}, w: &recWriter{}, consult: &fakeConsult{}}
	r.mgr = &fakeMgr{st: r.st, w: r.w, auth: true}
	r.agent = &agentstore.Agent{ID: "a1", Name: "a1", AiCli: "antigravity", Workdir: workdir,
		PermissionPosture: posture, AutoApprove: act, Status: store.StatusWorking}
	ag := &fakeAgents{a: r.agent}
	be := backends.Antigravity{}
	cfg := stateclassifier.DefaultConfig()
	cfg.QuiescenceGrace = time.Hour // investigations are driven explicitly
	cfg.Hysteresis = ptyevent.Hysteresis{
		Dwell:      map[ptyevent.State]time.Duration{},
		MinHits:    map[ptyevent.State]int{},
		PendingTTL: time.Minute, IdleQuiet: time.Millisecond,
	}
	r.cls = stateclassifier.New(cfg, r.mgr, ag, func(*agentstore.Agent) agentbackend.Backend { return be },
		fakeProcs{info: stateclassifier.ProcessInfo{RootAlive: true}}, nil)
	t.Cleanup(r.cls.Close)

	shapes := decisionrouter.NewMemoryLearnedShapeStore()
	pc := decisionrouter.NewPromptClassifierWith(shapes, nil)
	router := decisionrouter.New(decisionrouter.RouterOptions{Classifier: pc, ShapeStore: shapes, Consultor: r.consult})
	ecfg := Config{
		Settle: time.Hour, Retry: time.Millisecond,
		Policy:  func(*agentstore.Agent) approval.Policy { return approval.Policy{} },
		Backend: func(*agentstore.Agent) agentbackend.Backend { return be },
	}
	if mutate != nil {
		mutate(&ecfg)
	}
	r.cls.Metrics = ecfg.Metrics
	r.eng = New(ecfg, r.mgr, ag, router, pc, nil)
	r.cls.OnPrompt = r.eng.OnPrompt
	r.cls.OnUnreadable = r.eng.OnUnreadable
	return r
}

// look shows screen and lets the classifier investigate it once.
func (r *rig) look(screens ...string) {
	r.st.set(screens...)
	r.cls.OnPTYEvent("a1", ptyevent.Event{Kind: ptyevent.KindOutput, Output: &ptyevent.Output{Data: []byte("x")}})
	r.cls.Investigate(context.Background(), "a1")
}

func (r *rig) waitDecision(n int) []*ptyevent.DecisionEvent {
	r.t.Helper()
	for i := 0; i < 400; i++ {
		if d := r.st.decisions(); len(d) >= n {
			return d
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.t.Fatalf("no decision journaled; have %v", r.st.decisions())
	return nil
}

func (r *rig) settle() { time.Sleep(60 * time.Millisecond) }

func (r *rig) state() ptyevent.State {
	s, _ := r.cls.CurrentState("a1")
	return s.State
}

var (
	cmdPane   = func(t *testing.T) string { return fixture(t, "approval-run-command.txt") }
	trustPane = func(t *testing.T) string { return fixture(t, "trust-prompt.txt") }
)

func TestE2EActiveOutputQuietIdleAndExternalWait(t *testing.T) {
	r := newRig(t, ptyevent.PostureDangerouslyExecuteAll, true, "/w")
	r.cls.OnPTYEvent("a1", ptyevent.Event{Kind: ptyevent.KindLifecycle, Lifecycle: &ptyevent.Lifecycle{Phase: ptyevent.PhaseSpawned, PID: 4242}})
	r.look(fixture(t, "state-working.txt"))
	if r.state() != ptyevent.StateWorking {
		t.Fatalf("active output: state = %s", r.state())
	}
	r.look(fixture(t, "state-idle.txt"))
	r.look(fixture(t, "state-idle.txt"))
	if r.state() != ptyevent.StateIdle {
		t.Fatalf("completion: state = %s", r.state())
	}
	r.look(fixture(t, "rate-limit.txt"))
	if s := r.state(); s != ptyevent.StateWaiting {
		t.Fatalf("external wait: state = %s", s)
	}
	r.settle()
	if got := r.w.all(); len(got) != 0 {
		t.Fatalf("non-prompt screens must never receive input, got %q", got)
	}
}

func TestE2EKnownPermissionByPosture(t *testing.T) {
	cases := []struct {
		name    string
		posture ptyevent.Posture
		act     bool
		want    []string // keystrokes written
		action  ptyevent.Action
	}{
		// option 1 is the cursor default: Enter alone.
		{"dangerous approves least-privilege allow", ptyevent.PostureDangerouslyExecuteAll, true, []string{"\r"}, ptyevent.ActionApprove},
		// execute is forbidden read-only: deny = option 4, three Downs then Enter.
		{"read-only denies execute", ptyevent.PostureReadOnly, true, []string{"\x1b[B\x1b[B\x1b[B\r"}, ptyevent.ActionDeny},
		{"read-write asks a human", ptyevent.PostureReadWrite, true, nil, ptyevent.ActionAskHuman},
		{"act=false never writes", ptyevent.PostureDangerouslyExecuteAll, false, nil, ptyevent.ActionAskHuman},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, tc.posture, tc.act, "/w")
			r.look(cmdPane(t))
			d := r.waitDecision(1)[0]
			if d.Action != tc.action {
				t.Fatalf("action = %s (%s), want %s", d.Action, d.Reason, tc.action)
			}
			r.settle()
			got := r.w.all()
			if len(got) != len(tc.want) || (len(got) == 1 && got[0] != tc.want[0]) {
				t.Fatalf("keystrokes = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestE2ETrustPrompt(t *testing.T) {
	scratch := "/tmp/claude-1000/-home-srjn45-dev-warden--worktrees-development-ec875845/9d3d7661-b366-43d4-84c0-ed70771b32b9/scratchpad/agytrust.dHzcZl"
	for _, tc := range []struct {
		name, workdir string
		posture       ptyevent.Posture
		wantWrite     bool
	}{
		{"dangerous trusts any folder", "/somewhere/else", ptyevent.PostureDangerouslyExecuteAll, true},
		{"read-write trusts own worktree", scratch, ptyevent.PostureReadWrite, true},
		{"read-write does not trust foreign dir", "/somewhere/else", ptyevent.PostureReadWrite, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, tc.posture, true, tc.workdir)
			r.look(trustPane(t))
			r.waitDecision(1)
			r.settle()
			if wrote := len(r.w.all()) > 0; wrote != tc.wantWrite {
				t.Fatalf("wrote = %v (%q), want %v", wrote, r.w.all(), tc.wantWrite)
			}
		})
	}
}

const planMenu = `Here is my plan.

Which approach should I take?
> 1. Rewrite the module
  2. Patch in place

  ↑/↓ Navigate · enter Confirm
`

func TestE2EStrategicMenuIsNeverAPermission(t *testing.T) {
	r := newRig(t, ptyevent.PostureDangerouslyExecuteAll, true, "/w")
	// agy's parser does not read this menu: it is unreadable, so no recognizer
	// means needs_attention and nothing is typed.
	r.look(planMenu)
	time.Sleep(60 * time.Millisecond)
	if got := r.w.all(); len(got) != 0 {
		t.Fatalf("a strategic menu must not be answered by posture, wrote %q", got)
	}
	if r.consult.n() != 0 {
		t.Fatalf("non-autopilot sessions never consult the brain")
	}
}

func TestE2EDriftedPermissionNeedsAttention(t *testing.T) {
	r := newRig(t, ptyevent.PostureDangerouslyExecuteAll, true, "/w")
	drift := strings.NewReplacer("Yes, run command", "Go ahead", "No, cancel", "Stop").Replace(cmdPane(t))
	r.look(drift)
	d := r.waitDecision(1)[0]
	if d.Action == ptyevent.ActionApprove || d.Action == ptyevent.ActionDeny {
		t.Fatalf("drifted wording acted on: %+v", d)
	}
	r.settle()
	if got := r.w.all(); len(got) != 0 {
		t.Fatalf("drifted menu received input %q", got)
	}
}

func TestE2EStalePromptTOCTOU(t *testing.T) {
	r := newRig(t, ptyevent.PostureDangerouslyExecuteAll, true, "/w")
	other := strings.ReplaceAll(cmdPane(t), "git rev-parse HEAD origin/main", "rm -rf /")
	// Calls: classifier investigation (original), engine screen snapshot
	// (original), router's re-read just before writing (a DIFFERENT command in
	// the same box).
	r.look(cmdPane(t), cmdPane(t), other)
	d := r.waitDecision(1)[0]
	if d.Revalidation != ptyevent.RevalTargetChanged {
		t.Fatalf("revalidation = %q (%s), want target-changed", d.Revalidation, d.Reason)
	}
	r.settle()
	if got := r.w.all(); len(got) != 0 {
		t.Fatalf("a changed prompt must not be answered, wrote %q", got)
	}

	// And a prompt that vanished entirely.
	r2 := newRig(t, ptyevent.PostureDangerouslyExecuteAll, true, "/w")
	r2.look(cmdPane(t), cmdPane(t), fixture(t, "state-idle.txt"))
	if d := r2.waitDecision(1)[0]; d.Revalidation != ptyevent.RevalPromptGone {
		t.Fatalf("revalidation = %q, want prompt-gone", d.Revalidation)
	}
	r2.settle()
	if got := r2.w.all(); len(got) != 0 {
		t.Fatalf("a vanished prompt must not be answered, wrote %q", got)
	}
}

func TestE2EAnsweredOnceAcrossReconnect(t *testing.T) {
	r := newRig(t, ptyevent.PostureDangerouslyExecuteAll, true, "/w")
	r.look(cmdPane(t))
	r.waitDecision(1)
	r.settle()
	// The prompt is still painted (slow redraw / reconnect): same instance,
	// inside the settle window → not answered again.
	r.look(cmdPane(t))
	r.look(cmdPane(t))
	r.settle()
	if got := r.w.all(); len(got) != 1 {
		t.Fatalf("one prompt instance answered %d times: %q", len(got), got)
	}
	// Daemon restart: a recovered epoch plus a fresh engine on the same screen
	// decides from fresh evidence, exactly once.
	r.cls.OnPTYEvent("a1", ptyevent.Event{Kind: ptyevent.KindLifecycle, Lifecycle: &ptyevent.Lifecycle{Phase: ptyevent.PhaseRecovered, PID: 4242}})
	r2 := newRig(t, ptyevent.PostureDangerouslyExecuteAll, true, "/w")
	r2.look(cmdPane(t))
	r2.waitDecision(1)
	r2.settle()
	if got := r2.w.all(); len(got) != 1 {
		t.Fatalf("restarted engine answered %d times", len(got))
	}
}

func TestE2EShadowModeWritesNothing(t *testing.T) {
	r := newRig(t, ptyevent.PostureDangerouslyExecuteAll, true, "/w")
	r.mgr.auth = false
	r.look(cmdPane(t))
	d := r.waitDecision(1)[0]
	if !strings.Contains(d.Reason, "dry-run") {
		t.Fatalf("shadow decision must be marked dry-run: %+v", d)
	}
	r.settle()
	if got := r.w.all(); len(got) != 0 {
		t.Fatalf("shadow mode wrote %q", got)
	}
}

func TestE2EPostureBypassLaunch(t *testing.T) {
	b := backends.Antigravity{}
	sup := agentbackend.SupportForPosture(b, ptyevent.PostureDangerouslyExecuteAll)
	if !sup.Native || sup.Degraded || sup.Mode != "dangerously-skip-permissions" {
		t.Fatalf("support = %+v", sup)
	}
	if cmd := b.LaunchCmd(agentbackend.LaunchOpts{Mode: sup.Mode}); !strings.Contains(cmd, "--dangerously-skip-permissions") {
		t.Fatalf("launch = %q", cmd)
	}
}

func TestTmuxKeys(t *testing.T) {
	got, err := tmuxKeys([]byte("\x1b[B\x1b[A2\r"))
	if err != nil || strings.Join(got, ",") != "Down,Up,2,Enter" {
		t.Fatalf("keys = %v err=%v", got, err)
	}
	if _, err := tmuxKeys([]byte("\x03")); err == nil {
		t.Fatalf("control bytes must be refused")
	}
}
