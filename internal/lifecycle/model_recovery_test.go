package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/router"
	"github.com/stretchr/testify/require"
)

// rejectingCodex wraps the real codex adapter with a TEST-ONLY detector. No
// production adapter implements LaunchRejectionDetector (no recorded fixture
// exists yet), so these tests exercise the coordinator with a fake signature:
// anchored at line start, naming the model, versioned by the fixture header.
type rejectingCodex struct {
	agentbackend.Backend
	notPrePrompt bool
}

func (r rejectingCodex) RejectsBeforePrompt() bool { return !r.notPrePrompt }

var fakeRejectRe = regexp.MustCompile(`(?m)^fake-cli v1: model '([^']+)' is not available for this account$`)

func (rejectingCodex) DetectModelRejection(pane, model string) (string, bool) {
	m := fakeRejectRe.FindStringSubmatch(pane)
	if m == nil || m[1] != model {
		return "", false
	}
	return "model " + m[1] + " not available for account", true
}

// paneRunner is a deterministic fake tmux: the model on the most recent launch
// line decides what that session's pane shows and whether the CLI exited.
type paneRunner struct {
	mu       sync.Mutex
	exitsDir string
	// script maps a model id to its pane text; exit>0 entries write an exit file.
	script map[string]paneScript
	launch map[string][]string // session -> models launched, in order
	panes  map[string]string
	kills  map[string]int
	sends  int
}

type paneScript struct {
	pane string
	exit int // 0: still running (no exit file)
}

var modelFlagRe = regexp.MustCompile(` -m '?([^' ]+)'?`)

func newPaneRunner(exitsDir string, script map[string]paneScript) *paneRunner {
	return &paneRunner{exitsDir: exitsDir, script: script, launch: map[string][]string{}, panes: map[string]string{}, kills: map[string]int{}}
}

func (p *paneRunner) Run(_ context.Context, _ string, name string, args ...string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if name != "tmux" || len(args) == 0 {
		return "", nil
	}
	switch args[0] {
	case "send-keys":
		sess, line := args[2], args[3]
		if !strings.Contains(line, "codex") {
			return "", nil
		}
		p.sends++
		m := modelFlagRe.FindStringSubmatch(line)
		model := ""
		if m != nil {
			model = m[1]
		}
		p.launch[sess] = append(p.launch[sess], model)
		sc := p.script[model]
		p.panes[sess] = sc.pane
		if sc.exit > 0 {
			_ = os.WriteFile(filepath.Join(p.exitsDir, sess), []byte("1"), 0o600)
		} else {
			_ = os.Remove(filepath.Join(p.exitsDir, sess))
		}
	case "capture-pane":
		return p.panes[args[len(args)-1]], nil
	case "kill-session":
		p.kills[args[len(args)-1]]++
	}
	return "", nil
}

const rejectPane = "fake-cli v1: model '%s' is not available for this account\n$ "

func rejected(model string) paneScript {
	return paneScript{pane: strings.Replace(rejectPane, "%s", model, 1), exit: 1}
}

type recorder struct {
	inner LaunchEvidenceRecorder
	mu    sync.Mutex
	calls []string
}

func (r *recorder) RecordLaunchEvidence(a, m string, ev backendstore.LaunchEvidence, d time.Duration) error {
	r.mu.Lock()
	r.calls = append(r.calls, a+"/"+m)
	r.mu.Unlock()
	return r.inner.RecordLaunchEvidence(a, m, ev, d)
}

type recoveryRig struct {
	lc  *Lifecycle
	run *paneRunner
	st  *backendstore.Store
	rec *recorder
	cwd string
}

func newRecoveryRig(t *testing.T, models []string, script map[string]paneScript) *recoveryRig {
	t.Helper()
	orig, err := agentbackend.Get("codex")
	require.NoError(t, err)
	agentbackend.Register(rejectingCodex{Backend: orig})
	t.Cleanup(func() { agentbackend.Register(orig) })

	st := seededCatalogStore(t)
	t.Cleanup(func() { st.Close() })
	seeded, err := st.ListModels(backendstore.ModelTier(""))
	require.NoError(t, err)
	for _, m := range seeded {
		if m.BackendID == "codex" {
			require.NoError(t, st.SetModelEnabled("codex", m.ModelID, false))
		}
	}
	for _, m := range models {
		require.NoError(t, st.UpsertModel(backendstore.ModelEntry{BackendID: "codex", ModelID: m, DisplayName: m, Tier: backendstore.Tier2, Enabled: true, AutoAssign: true, QuotaScope: "codex"}))
	}
	require.NoError(t, st.PutCatalogScope(backendstore.ActiveCatalogScope{AiCli: "codex", ActiveScopeFP: "scope"}))

	dir := t.TempDir()
	exits := filepath.Join(dir, "exits")
	require.NoError(t, os.MkdirAll(exits, 0o700))
	run := newPaneRunner(exits, script)
	lc := New(run, &FakeConfig{})
	lc.ExitsDir = exits
	lc.PromptsDir = filepath.Join(dir, "prompts")
	lc.Resolver = router.NewResolver(st)
	rec := &recorder{inner: st}
	lc.Evidence = rec
	lc.recoveryPoll = time.Millisecond
	lc.ModelRecovery = ModelRecoveryConfig{AttemptTimeout: 300 * time.Millisecond, Deadline: 5 * time.Second}
	cwd := filepath.Join(dir, "work")
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	return &recoveryRig{lc: lc, run: run, st: st, rec: rec, cwd: cwd}
}

func (r *recoveryRig) spawn(model string) (string, error) {
	a, err := r.lc.Spawn(context.Background(), SpawnRequest{
		Cwd: r.cwd, Backend: "codex", Model: model, Tier: "tier-2", Name: "rec", Prompt: "do the task",
	})
	if err != nil {
		return "", err
	}
	return a.Model, nil
}

func (r *recoveryRig) launched() []string {
	r.run.mu.Lock()
	defer r.run.mu.Unlock()
	for _, v := range r.run.launch {
		return append([]string(nil), v...)
	}
	return nil
}

func (r *recoveryRig) observation(t *testing.T, model string) backendstore.StoredObservation {
	o, err := r.st.GetCatalogObservation("codex", "scope", model)
	require.NoError(t, err)
	return o
}

func TestStartupRecoveryObsoleteModelFallsBackWithinAiCliAndTier(t *testing.T) {
	r := newRecoveryRig(t, []string{"gpt-5.5", "m-b", "m-c"}, map[string]paneScript{
		"gpt-5.5": rejected("gpt-5.5"), "m-b": rejected("m-b"),
		"m-c": {pane: "codex ready"},
	})
	got, err := r.spawn("")
	require.NoError(t, err)
	l := r.launched()
	require.LessOrEqual(t, len(l), 3)
	require.Equal(t, got, l[len(l)-1])
	seen := map[string]bool{}
	for _, m := range l {
		require.False(t, seen[m], "pair repeated: %v", l)
		seen[m] = true
	}
	require.Equal(t, "m-c", got, "the only startable model must be reached within 3 distinct attempts: %v", l)
	for _, bad := range []string{"gpt-5.5", "m-b"} {
		if seen[bad] {
			o := r.observation(t, bad)
			require.Equal(t, "invalid_model", o.LaunchRejectionReason)
			require.True(t, o.LaunchRejectedUntil.After(time.Now()))
		}
	}
	require.Equal(t, len(l)-1, len(r.rec.calls), "one negative-evidence write per rejected attempt")
	for _, n := range r.run.kills {
		require.GreaterOrEqual(t, n, len(l)-1, "each failed child's pane is reaped")
	}
}

func TestStartupRecoveryExhaustsAfterThreeDistinctAttempts(t *testing.T) {
	r := newRecoveryRig(t, []string{"a", "b", "c", "d", "e"}, map[string]paneScript{
		"a": rejected("a"), "b": rejected("b"), "c": rejected("c"), "d": rejected("d"), "e": rejected("e"),
	})
	_, err := r.spawn("")
	var ex *ModelRecoveryExhaustedError
	require.True(t, errors.As(err, &ex), "got %v", err)
	require.Len(t, ex.Attempts, 3)
	require.Len(t, r.launched(), 3)
	require.Len(t, r.rec.calls, 3)
	require.True(t, IsModelRecoveryError(err))
}

func TestStartupRecoveryNoAlternativeIsExhaustedTyped(t *testing.T) {
	r := newRecoveryRig(t, []string{"only"}, map[string]paneScript{"only": rejected("only")})
	_, err := r.spawn("")
	var ex *ModelRecoveryExhaustedError
	require.True(t, errors.As(err, &ex), "got %v", err)
	require.Len(t, r.launched(), 1, "no other eligible same-aicli same-tier model: no relaunch")
}

func TestStartupRecoveryExactPinIsReportedNotSubstituted(t *testing.T) {
	r := newRecoveryRig(t, []string{"pin", "other"}, map[string]paneScript{
		"pin": rejected("pin"), "other": {pane: "codex ready"},
	})
	_, err := r.spawn("pin")
	var rej *ModelRejectedError
	require.True(t, errors.As(err, &rej), "got %v", err)
	require.Equal(t, "pin", rej.Model)
	require.Equal(t, []string{"pin"}, r.launched(), "a pin is never re-routed")
	require.Equal(t, []string{"codex/pin"}, r.rec.calls)
}

func TestStartupRecoveryMisleadingOutputIsNotInvalidModel(t *testing.T) {
	cases := map[string]paneScript{
		"unanchored mention":   {pane: "note: fake-cli v1: model 'x' is not available for this account\n", exit: 1},
		"rate limit 429":       {pane: "429 Too Many Requests: model x\n", exit: 1},
		"auth":                 {pane: "authentication required\n", exit: 1},
		"network":              {pane: "connection refused while loading model x\n", exit: 1},
		"clean exit w/ phrase": {pane: "fake-cli v1: model 'x' is not available for this account\n", exit: 0},
	}
	for name, sc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRecoveryRig(t, []string{"x", "y"}, map[string]paneScript{"x": sc, "y": {pane: "codex ready"}})
			_, err := r.spawn("")
			require.NoError(t, err)
			require.Len(t, r.launched(), 1, "no relaunch for non-invalid-model output")
			require.Empty(t, r.rec.calls, "no negative evidence recorded")
		})
	}
}

func TestStartupRecoveryLateErrorAfterPromptIsNotClassified(t *testing.T) {
	// The CLI is running (no exit file) and prints the signature later, as it
	// would mid-task: the window ends without a verdict and nothing is recorded.
	r := newRecoveryRig(t, []string{"x", "y"}, map[string]paneScript{
		"x": {pane: "working...\nfake-cli v1: model 'x' is not available for this account\n"}, "y": {pane: "ok"},
	})
	r.lc.ModelRecovery.AttemptTimeout = 30 * time.Millisecond
	_, err := r.spawn("")
	require.NoError(t, err)
	require.Len(t, r.launched(), 1)
	require.Empty(t, r.rec.calls)
}

func TestStartupRecoveryEvidenceSurvivesRestartAndBlocksReroute(t *testing.T) {
	r := newRecoveryRig(t, []string{"gone", "ok"}, map[string]paneScript{"gone": rejected("gone"), "ok": {pane: "ready"}})
	_, err := r.spawn("gone")
	require.Error(t, err)
	// New resolver over the same store = daemon restart: the persisted
	// expiring observation still removes the model from routing.
	res, err := router.NewResolver(r.st).Resolve(context.Background(), router.ResolveOptions{
		Tier: backendstore.Tier2, PreferredBackend: "codex",
	})
	require.NoError(t, err)
	require.Equal(t, "ok", res.ModelID)
	// Past the TTL it is routable again.
	future := time.Now().Add(48 * time.Hour)
	res, err = router.NewResolver(r.st).WithNow(func() time.Time { return future }).Resolve(context.Background(), router.ResolveOptions{
		Tier: backendstore.Tier2, PreferredBackend: "codex", PreferredModel: "gone",
	})
	require.NoError(t, err)
	require.Equal(t, "gone", res.ModelID)
}

func TestStartupRecoveryConcurrentLaunchesStayBounded(t *testing.T) {
	r := newRecoveryRig(t, []string{"a", "b", "c", "d"}, map[string]paneScript{
		"a": rejected("a"), "b": rejected("b"), "c": rejected("c"), "d": {pane: "ready"},
	})
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = r.lc.Spawn(context.Background(), SpawnRequest{
				Cwd: r.cwd, Backend: "codex", Tier: "tier-2", Name: "rec" + string(rune('a'+i)), Prompt: "p",
			})
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			var ex *ModelRecoveryExhaustedError
			require.True(t, errors.As(err, &ex), "only typed exhaustion is acceptable: %v", err)
			require.LessOrEqual(t, len(ex.Attempts), 3)
		}
	}
	r.run.mu.Lock()
	defer r.run.mu.Unlock()
	for sess, l := range r.run.launch {
		require.LessOrEqual(t, len(l), 3, sess)
	}
}

func TestStartupRecoveryNoDetectorMeansNoRecovery(t *testing.T) {
	// Production adapters carry no detector: a rejected-looking pane is ignored.
	r := newRecoveryRig(t, []string{"x"}, map[string]paneScript{"x": rejected("x")})
	orig, _ := agentbackend.Get("codex")
	agentbackend.Register(orig.(rejectingCodex).Backend)
	_, err := r.spawn("")
	require.NoError(t, err)
	require.Len(t, r.launched(), 1)
	require.Empty(t, r.rec.calls)
}

func TestStartupRecoveryFailsClosedWithoutPrePromptAttestation(t *testing.T) {
	r := newRecoveryRig(t, []string{"x", "y"}, map[string]paneScript{"x": rejected("x"), "y": {pane: "ready"}})
	orig, _ := agentbackend.Get("codex")
	agentbackend.Register(rejectingCodex{Backend: orig.(rejectingCodex).Backend, notPrePrompt: true})
	_, err := r.spawn("")
	require.NoError(t, err)
	require.Len(t, r.launched(), 1, "prompt may have been accepted: no retry")
	require.Empty(t, r.rec.calls)
}

func TestModelRecoveryAttemptCapIsThree(t *testing.T) {
	require.Equal(t, 3, ModelRecoveryConfig{MaxAttempts: 5}.norm().MaxAttempts)
	require.Equal(t, 2, ModelRecoveryConfig{MaxAttempts: 2}.norm().MaxAttempts)
	require.Equal(t, 3, ModelRecoveryConfig{}.norm().MaxAttempts)
}

func TestStartupRecoveryRejectionMustNameAttemptedModel(t *testing.T) {
	other := paneScript{pane: "fake-cli v1: model 'someone-else' is not available for this account\n$ ", exit: 1}
	r := newRecoveryRig(t, []string{"x", "y"}, map[string]paneScript{"x": other, "y": other})
	_, err := r.spawn("")
	require.NoError(t, err)
	require.Len(t, r.launched(), 1)
	require.Empty(t, r.rec.calls)
}
