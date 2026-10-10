package fastbrain

// Deterministic baseline fixtures for the Fast-Brain admission/SLO design
// (docs/specs/2026-10-10-fast-brain-decision-inventory-and-slos.md). They pin
// DELIVERED behaviour of the engine as of #775/#785/#858 — including its known
// gaps — so the follow-up admission work changes them deliberately. Nothing
// here asserts a desired end state; a test named *Gap documents a limitation
// and is expected to be rewritten when the limitation is fixed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentbackend"
)

type incidentCall struct {
	Kind DecisionKind `json:"kind"`
	Tier Tier         `json:"tier"`
}

type incidentFixture struct {
	Engine struct {
		MaxConcurrent     int `json:"max_concurrent"`
		FastTimeoutMS     int `json:"fast_timeout_ms"`
		ThinkingTimeoutMS int `json:"thinking_timeout_ms"`
	} `json:"engine"`
	Stampede struct {
		Kind            DecisionKind `json:"kind"`
		DistinctPrompts int          `json:"distinct_prompts"`
	} `json:"stampede"`
	Starvation struct {
		Occupiers []incidentCall `json:"occupiers"`
		Victim    struct {
			incidentCall
			CallerDeadlineMS int `json:"caller_deadline_ms"`
		} `json:"victim"`
	} `json:"starvation"`
}

func loadIncident(t *testing.T) incidentFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "incident_2026_10_timeout.json"))
	require.NoError(t, err)
	var f incidentFixture
	require.NoError(t, json.Unmarshal(raw, &f))
	return f
}

func (f incidentFixture) options() EngineOptions {
	return EngineOptions{
		MaxConcurrent:   f.Engine.MaxConcurrent,
		FastTimeout:     time.Duration(f.Engine.FastTimeoutMS) * time.Millisecond,
		ThinkingTimeout: time.Duration(f.Engine.ThinkingTimeoutMS) * time.Millisecond,
	}
}

// promptRecorder is a runner that records every prompt it is started with and
// blocks until released or its context ends (a "cold native CLI").
type promptRecorder struct {
	mu      sync.Mutex
	prompts []string
	started chan string
	release chan struct{}
}

func newPromptRecorder() *promptRecorder {
	return &promptRecorder{started: make(chan string, 256), release: make(chan struct{})}
}

func (p *promptRecorder) Run(ctx context.Context, prompt string) (string, error) {
	p.mu.Lock()
	p.prompts = append(p.prompts, prompt)
	p.mu.Unlock()
	p.started <- prompt
	select {
	case <-p.release:
		return `{"ok":true}`, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (p *promptRecorder) count(prefix string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, s := range p.prompts {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

func waitStarted(t *testing.T, p *promptRecorder, n int) {
	t.Helper()
	for i := range n {
		select {
		case <-p.started:
		case <-time.After(2 * time.Second):
			t.Fatalf("runner start %d/%d never happened", i+1, n)
		}
	}
}

// #858 regression guard: a burst of cosmetic summaries may occupy at most one
// slot and every other one is deferred without starting a process, so an
// operational decision still gets a runner immediately.
func TestIncidentCosmeticStampedeLeavesCapacityForOperationalDecisions(t *testing.T) {
	f := loadIncident(t)
	rec := newPromptRecorder()
	eng := NewEngineWithOptions(rec, rec, f.options())

	var wg sync.WaitGroup
	var deferred, other atomic.Int32
	for i := range f.Stampede.DistinctPrompts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, _ := eng.Decide(context.Background(), Request{
				Kind: f.Stampede.Kind, Tier: TierFast, Prompt: fmt.Sprintf("act-%02d badge", i),
			})
			if r.Status == StatusDeferred {
				deferred.Add(1)
			} else {
				other.Add(1)
			}
		}(i)
	}
	waitStarted(t, rec, 1)
	// Every other burst member must be shed before the holder is released,
	// otherwise a late goroutine would legitimately be admitted.
	require.Eventually(t, func() bool { return deferred.Load() == int32(f.Stampede.DistinctPrompts-1) },
		time.Second, time.Millisecond)
	require.Equal(t, 1, rec.count("act-"), "at most one cosmetic call may hold a slot")

	// The operational decision starts a runner while the cosmetic one is parked.
	done := make(chan Response, 1)
	go func() {
		r, _ := eng.Decide(context.Background(), Request{Kind: KindArbitrateApproval, Tier: TierFast, Prompt: "arb"})
		done <- r
	}()
	waitStarted(t, rec, 1)
	require.Equal(t, 1, rec.count("arb"))
	close(rec.release)
	require.True(t, (<-done).OK())
	wg.Wait()
	require.Equal(t, int32(f.Stampede.DistinctPrompts-1), deferred.Load())
	require.Equal(t, int32(1), other.Load())
}

// Formerly gap G1 (semaphore-only admission): operational/best-effort work
// could hold every slot and starve a permission arbitration. P1 now owns a
// reserved slot, so the victim reaches the runner immediately and the
// best-effort occupier is shed rather than queued.
func TestIncidentArbitrationReachesRunnerDespiteOccupiers(t *testing.T) {
	f := loadIncident(t)
	rec := newPromptRecorder()
	eng := NewEngineWithOptions(rec, rec, f.options())

	var wg sync.WaitGroup
	var deferred atomic.Int32
	for i, o := range f.Starvation.Occupiers {
		wg.Add(1)
		go func(i int, o incidentCall) {
			defer wg.Done()
			r, _ := eng.Decide(context.Background(), Request{Kind: o.Kind, Tier: o.Tier, Prompt: fmt.Sprintf("occ-%d", i)})
			if r.Status == StatusDeferred {
				deferred.Add(1)
			}
		}(i, o)
		if i == 0 {
			waitStarted(t, rec, 1) // operational occupier takes the slot first
		}
	}
	// One non-P1 slot exists: the best-effort occupier is shed.
	require.Eventually(t, func() bool { return deferred.Load() == 1 }, time.Second, time.Millisecond)

	v := f.Starvation.Victim
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(v.CallerDeadlineMS)*time.Millisecond)
	defer cancel()
	done := make(chan Response, 1)
	go func() {
		r, _ := eng.Decide(ctx, Request{Kind: v.Kind, Tier: v.Tier, Prompt: "victim-arbitration"})
		done <- r
	}()
	waitStarted(t, rec, 1)
	require.Equal(t, 1, rec.count("victim"), "the model is consulted immediately")
	close(rec.release)
	r := <-done
	require.True(t, r.OK())
	require.NotZero(t, r.Duration, "Duration is now end-to-end (gap G7 for the response value)")
	require.Equal(t, ClassSafety, r.Admission.Class)
	wg.Wait()
}

// With a single slot the #858 reservation (active >= max-1) defers every
// cosmetic call, so MaxConcurrent=1 means "activity summaries never run".
func TestActivityNeverRunsWhenMaxConcurrentIsOne(t *testing.T) {
	var calls atomic.Int32
	eng := NewEngineWithOptions(RunnerFunc(func(context.Context, string) (string, error) {
		calls.Add(1)
		return `{"summary":"x"}`, nil
	}), nil, EngineOptions{MaxConcurrent: 1})
	r, err := eng.Decide(context.Background(), Request{Kind: KindSummarizeActivity, Tier: TierFast, Prompt: "a"})
	require.NoError(t, err)
	require.Equal(t, StatusDeferred, r.Status)
	require.Zero(t, calls.Load())
}

// Formerly gap G4: a coalesced follower no longer inherits the leader's
// context. The shared call is engine-owned and survives the leader leaving.
func TestCoalescedFollowerSurvivesLeaderCancellation(t *testing.T) {
	rec := newPromptRecorder()
	eng := NewEngineWithOptions(rec, nil, EngineOptions{MaxConcurrent: 2})
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leader := make(chan Response, 1)
	go func() {
		r, _ := eng.Decide(leaderCtx, Request{Kind: KindArbitrateApproval, Tier: TierFast, Prompt: "same"})
		leader <- r
	}()
	waitStarted(t, rec, 1)

	followerCtx, cancelFollower := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFollower()
	follower := make(chan Response, 1)
	go func() {
		r, _ := eng.Decide(followerCtx, Request{Kind: KindArbitrateApproval, Tier: TierFast, Prompt: "same"})
		follower <- r
	}()
	require.Eventually(t, func() bool {
		return eng.(interface{ AdmissionSnapshot() AdmissionSnapshot }).AdmissionSnapshot().Coalesced == 1
	}, time.Second, time.Millisecond)
	cancelLeader()

	require.Equal(t, StatusCanceled, (<-leader).Status)
	close(rec.release)
	fr := <-follower
	require.True(t, fr.OK(), "follower gets the shared result, got %s", fr.Status)
	require.True(t, fr.Admission.Coalesced)
	require.Equal(t, 1, rec.count("same"))
}

// Formerly the cache half of gap G3: pure-content kinds are served from a
// bounded result cache, and terminal failures are remembered briefly so a dead
// provider is not re-invoked per request. (A per-runner circuit/health state
// remains runner-cancellation-and-health work.)
func TestResultAndTerminalFailureAreCached(t *testing.T) {
	var ok, bad atomic.Int32
	eng := NewEngineWithOptions(
		RunnerFunc(func(_ context.Context, p string) (string, error) {
			if strings.HasPrefix(p, "ok") {
				ok.Add(1)
				return `{"a":1}`, nil
			}
			bad.Add(1)
			return "", errors.New("claude: unavailable")
		}), nil, EngineOptions{})
	for i := range 3 {
		r, _ := eng.Decide(context.Background(), Request{Kind: KindClassifyTask, Tier: TierFast, Prompt: "ok same"})
		require.True(t, r.OK())
		require.Equal(t, i > 0, r.Admission.Cached)
	}
	require.Equal(t, int32(1), ok.Load())
	for range 6 {
		r, _ := eng.Decide(context.Background(), Request{Kind: KindClassifyTask, Tier: TierFast, Prompt: "bad same"})
		require.Equal(t, StatusRunnerError, r.Status)
	}
	require.Equal(t, int32(1), bad.Load())
}

// Gap: distinct prompts of the same kind are never coalesced, so N agents
// asking N slightly different questions each cost a model call.
func TestDistinctPromptsAreNotCoalescedGap(t *testing.T) {
	var calls atomic.Int32
	eng := NewEngineWithOptions(RunnerFunc(func(context.Context, string) (string, error) {
		calls.Add(1)
		return `{"a":1}`, nil
	}), nil, EngineOptions{})
	for i := range 4 {
		_, _ = eng.Decide(context.Background(), Request{Kind: KindResolveAgentName, Tier: TierFast, Prompt: fmt.Sprintf("p%d", i)})
	}
	require.Equal(t, int32(4), calls.Load())
}

// Tiers still share one pool (by design: max_concurrent is a daemon-wide
// process bound), but a P1 thinking call is never queued behind a fast P3 one.
func TestTiersShareOnePoolButSafetyIsNotQueuedBehindOperational(t *testing.T) {
	rec := newPromptRecorder()
	eng := NewEngineWithOptions(rec, rec, EngineOptions{MaxConcurrent: 1, FastTimeout: time.Second})
	go func() {
		_, _ = eng.Decide(context.Background(), Request{Kind: KindCommitMessage, Tier: TierFast, Prompt: "f"})
	}()
	waitStarted(t, rec, 1)
	// max=1: the single slot is held by P3. P1 is the only class that may
	// still start (total < max is false) so it queues, and is admitted the
	// moment the slot frees — ahead of any later operational call.
	res := make(chan Response, 1)
	go func() {
		r, _ := eng.Decide(context.Background(), Request{Kind: KindRecognizePrompt, Tier: TierThinking, Prompt: "t"})
		res <- r
	}()
	require.Eventually(t, func() bool {
		return eng.(interface{ AdmissionSnapshot() AdmissionSnapshot }).AdmissionSnapshot().Queued[ClassSafety] == 1
	}, time.Second, time.Millisecond)
	close(rec.release)
	require.True(t, (<-res).OK())
}

// Privacy/bound baseline. Redaction + clipping are applied per prompt builder,
// not by the engine. Crash, stall and CI prompts redact; arbiter, recognition
// and the template kinds do not, and the arbiter prompt is unbounded. The
// design doc (§7) requires an engine-level redact+bound; when that lands these
// expectations flip.
func TestPromptRedactionAndBoundBaselineGap(t *testing.T) {
	secret := "sk-" + strings.Repeat("a1B2", 8)
	leaks := func(s string) bool { return strings.Contains(s, secret) }

	require.True(t, leaks(toolPermissionPrompt(ArbiterInput{
		Approval: &agentbackend.Approval{Action: "Bash(curl -H 'x: " + secret + "')", Question: "Proceed?"},
	})), "arbiter prompt currently carries secrets verbatim")
	require.True(t, leaks(recognizePrompt("token "+secret+"\n1. Yes\n2. No")))
	require.True(t, leaks(ClassifyTaskPrompt(secret)))
	require.True(t, leaks(CommitMessagePrompt("+key = "+secret)))

	require.False(t, leaks(stallPrompt(StallInput{Pane: "key " + secret})))
	require.False(t, leaks(ciPrompt(CIInput{Check: "build", Log: "key " + secret})))
	require.False(t, leaks(crashPrompt(CrashInput{Excerpt: secret}, Sanitize(secret))))

	huge := strings.Repeat("x", 10*maxPromptInputBytes)
	p := toolPermissionPrompt(ArbiterInput{Approval: &agentbackend.Approval{Action: "Bash", Question: huge}})
	require.Greater(t, len(p), 10*maxPromptInputBytes, "arbiter payload is unbounded")
	require.LessOrEqual(t, len(ClassifyTaskPrompt(huge)), maxPromptInputBytes+len(classifyTaskSystem)+32)
	require.LessOrEqual(t, len(stallPrompt(StallInput{Pane: huge, Facts: huge, Header: huge})),
		3*maxPromptInputBytes+len(stallSystemFmt)+1024)
}

// Drift guard: every DecisionKind constant must appear in the inventory below
// and in the design doc, so a new kind cannot ship without a class, SLO and
// priority assignment.
var decisionInventory = map[DecisionKind]struct {
	Class    string
	Priority int
}{
	KindArbitrateApproval: {"safety-critical", 1},
	KindRecognizePrompt:   {"safety-critical", 1},
	KindReplTurn:          {"human-interactive", 2},
	KindDiagnoseStall:     {"high-value-event-driven", 2},
	KindDiagnoseFailure:   {"high-value-event-driven", 3},
	KindClassifyCIFailure: {"high-value-event-driven", 3},
	KindSummarizeCheck:    {"high-value-event-driven", 3},
	KindCommitMessage:     {"best-effort-cosmetic", 3},
	KindPRSummary:         {"best-effort-cosmetic", 3},
	KindRouteTier:         {"best-effort-cosmetic", 3},
	KindClassifyTask:      {"best-effort-cosmetic", 4},
	KindResolveAgentName:  {"best-effort-cosmetic", 4},
	KindSummarizeActivity: {"best-effort-cosmetic", 4},
	KindCurateExtract:     {"best-effort-cosmetic", 4},
}

func declaredKinds(t *testing.T) []DecisionKind {
	t.Helper()
	fset := token.NewFileSet()
	var out []DecisionKind
	for _, file := range []string{"types.go", "recognize.go"} {
		f, err := parser.ParseFile(fset, file, nil, 0)
		require.NoError(t, err)
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				id, ok := vs.Type.(*ast.Ident)
				if !ok || id.Name != "DecisionKind" || len(vs.Values) != 1 {
					continue
				}
				if bl, ok := vs.Values[0].(*ast.BasicLit); ok {
					out = append(out, DecisionKind(strings.Trim(bl.Value, `"`)))
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

func TestDecisionInventoryCoversEveryKind(t *testing.T) {
	kinds := declaredKinds(t)
	require.NotEmpty(t, kinds)
	for _, k := range kinds {
		_, ok := decisionInventory[k]
		require.True(t, ok, "kind %q has no entry in decisionInventory and the design doc", k)
	}
	require.Len(t, decisionInventory, len(kinds), "inventory has an entry for a kind that no longer exists")

	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "specs", "2026-10-10-fast-brain-decision-inventory-and-slos.md"))
	require.NoError(t, err)
	for _, k := range kinds {
		require.Contains(t, string(doc), "`"+string(k)+"`", "design doc is missing kind %s", k)
	}
}
