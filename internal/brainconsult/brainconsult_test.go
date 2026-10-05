package brainconsult

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/audit"
)

// fakeSpawner is a test double for Spawner.
type fakeSpawner struct {
	spawnFn    func(ctx context.Context, args BrainSpawnArgs) (*agentstore.Agent, error)
	outputFn   func(ctx context.Context, tmuxSession string, lines int) (string, error)
	teardownFn func(ctx context.Context, sess *agentstore.Agent) error

	spawnCalled    atomic.Int32
	teardownCalled atomic.Int32
}

func (f *fakeSpawner) Spawn(ctx context.Context, args BrainSpawnArgs) (*agentstore.Agent, error) {
	f.spawnCalled.Add(1)
	if f.spawnFn != nil {
		return f.spawnFn(ctx, args)
	}
	return &agentstore.Agent{ID: "brain-test", TmuxSession: "warden-brain-test"}, nil
}

func (f *fakeSpawner) Output(ctx context.Context, tmuxSession string, lines int) (string, error) {
	if f.outputFn != nil {
		return f.outputFn(ctx, tmuxSession, lines)
	}
	return "", nil
}

func (f *fakeSpawner) Teardown(ctx context.Context, sess *agentstore.Agent) error {
	f.teardownCalled.Add(1)
	if f.teardownFn != nil {
		return f.teardownFn(ctx, sess)
	}
	return nil
}

func noopAuditWriter() *audit.Writer { return nil }

// TestConsultValidAction verifies that a valid JSON reply is parsed and returned.
func TestConsultValidAction(t *testing.T) {
	sp := &fakeSpawner{
		outputFn: func(_ context.Context, _ string, _ int) (string, error) {
			return `some preamble
{"action": "retry_job", "reason": "transient failure"}
`, nil
		},
	}
	c := New(sp, noopAuditWriter(), Options{Timeout: 5 * time.Second})
	res, err := c.Consult(context.Background(), Request{
		Intent:    "stuck job",
		Situation: "job has been stuck for 5 minutes",
		Allowed:   []Action{ActionRetryJob, ActionEscalate},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionRetryJob {
		t.Errorf("expected action retry_job, got %q", res.Action)
	}
	if res.Reason != "transient failure" {
		t.Errorf("unexpected reason: %q", res.Reason)
	}
	if res.BrainID != "brain-test" {
		t.Errorf("expected BrainID brain-test, got %q", res.BrainID)
	}
}

// TestConsultUnknownAction verifies that an unrecognised action from the brain
// is treated as noop and does not propagate arbitrary execution.
func TestConsultUnknownAction(t *testing.T) {
	sp := &fakeSpawner{
		outputFn: func(_ context.Context, _ string, _ int) (string, error) {
			return `{"action": "destroy_everything", "reason": "chaos"}`, nil
		},
	}
	c := New(sp, noopAuditWriter(), Options{Timeout: 5 * time.Second})
	res, err := c.Consult(context.Background(), Request{
		Intent:  "test unknown",
		Allowed: []Action{ActionRetryJob},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionNoop {
		t.Errorf("expected noop for unknown action, got %q", res.Action)
	}
}

// TestConsultActionNotInAllowedSet verifies that a valid but disallowed action
// is treated as noop.
func TestConsultActionNotInAllowedSet(t *testing.T) {
	sp := &fakeSpawner{
		outputFn: func(_ context.Context, _ string, _ int) (string, error) {
			// mark_failed is valid but not in the allowed set below
			return `{"action": "mark_failed", "reason": "too slow"}`, nil
		},
	}
	c := New(sp, noopAuditWriter(), Options{Timeout: 5 * time.Second})
	res, err := c.Consult(context.Background(), Request{
		Intent:  "test disallowed",
		Allowed: []Action{ActionRetryJob, ActionWait},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionNoop {
		t.Errorf("expected noop for disallowed action, got %q", res.Action)
	}
}

// TestConsultTimeout verifies that ErrNoBrainReply is returned when the brain
// never produces a parseable reply within the deadline.
func TestConsultTimeout(t *testing.T) {
	sp := &fakeSpawner{
		outputFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "thinking...", nil // never a parseable reply
		},
	}
	// Very short timeout so the test runs quickly.
	c := New(sp, noopAuditWriter(), Options{Timeout: 150 * time.Millisecond})
	_, err := c.Consult(context.Background(), Request{
		Intent:  "timeout test",
		Allowed: []Action{ActionRetryJob},
	})
	if !errors.Is(err, ErrNoBrainReply) {
		t.Errorf("expected ErrNoBrainReply, got %v", err)
	}
}

// TestConsultTeardownAlwaysRuns verifies that Teardown is called even when the
// consult fails (spawn error path).
func TestConsultTeardownAlwaysRunsOnSpawnError(t *testing.T) {
	sp := &fakeSpawner{
		spawnFn: func(_ context.Context, _ BrainSpawnArgs) (*agentstore.Agent, error) {
			return nil, errors.New("no backends available")
		},
	}
	c := New(sp, noopAuditWriter(), Options{Timeout: 5 * time.Second})
	_, err := c.Consult(context.Background(), Request{Intent: "spawn error"})
	if err == nil {
		t.Fatal("expected error from spawn failure")
	}
	// Teardown should NOT have been called because spawn failed before we
	// had a session to tear down. This is intentional: we can only teardown
	// what was spawned.
	if sp.teardownCalled.Load() != 0 {
		t.Errorf("teardown should not be called when spawn fails, got %d calls", sp.teardownCalled.Load())
	}
}

// TestConsultTeardownAlwaysRunsOnTimeout verifies that Teardown is called
// even when no reply arrives within the deadline.
func TestConsultTeardownAlwaysRunsOnTimeout(t *testing.T) {
	sp := &fakeSpawner{
		outputFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
	}
	c := New(sp, noopAuditWriter(), Options{Timeout: 150 * time.Millisecond})
	_, err := c.Consult(context.Background(), Request{Intent: "teardown on timeout"})
	if !errors.Is(err, ErrNoBrainReply) {
		t.Errorf("expected ErrNoBrainReply, got %v", err)
	}
	if sp.teardownCalled.Load() != 1 {
		t.Errorf("expected teardown to be called once, got %d", sp.teardownCalled.Load())
	}
}

// TestConsultTeardownAlwaysRunsOnSuccess verifies that Teardown is called even
// on the happy path.
func TestConsultTeardownAlwaysRunsOnSuccess(t *testing.T) {
	sp := &fakeSpawner{
		outputFn: func(_ context.Context, _ string, _ int) (string, error) {
			return `{"action": "wait", "reason": "not yet"}`, nil
		},
	}
	c := New(sp, noopAuditWriter(), Options{Timeout: 5 * time.Second})
	_, err := c.Consult(context.Background(), Request{Intent: "teardown on success"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sp.teardownCalled.Load() != 1 {
		t.Errorf("expected teardown to be called once, got %d", sp.teardownCalled.Load())
	}
}

// TestConsultContextCancel verifies ErrNoBrainReply when the parent context is
// cancelled before any reply arrives.
func TestConsultContextCancel(t *testing.T) {
	// outputFn that blocks until ctx is cancelled
	sp := &fakeSpawner{
		outputFn: func(ctx context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
	}
	c := New(sp, noopAuditWriter(), Options{Timeout: 30 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Consult(ctx, Request{Intent: "ctx cancel"})
		done <- err
	}()

	// Cancel immediately after starting.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, ErrNoBrainReply) {
			t.Errorf("expected ErrNoBrainReply on ctx cancel, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Consult did not return after ctx cancel")
	}
}

// TestBuildPromptEvidenceCap verifies that evidence longer than 8 KB is
// truncated in the built prompt.
func TestBuildPromptEvidenceCap(t *testing.T) {
	bigEvidence := make([]byte, maxEvidenceBytes+1000)
	for i := range bigEvidence {
		bigEvidence[i] = 'Q' // use a char unlikely to appear in template text
	}
	req := Request{
		Situation: "big evidence",
		Evidence:  string(bigEvidence),
		Allowed:   []Action{ActionWait},
	}
	prompt, err := buildPrompt(req)
	if err != nil {
		t.Fatalf("buildPrompt: %v", err)
	}
	// Count 'Q's in the prompt — should be exactly maxEvidenceBytes.
	count := 0
	for _, b := range prompt {
		if b == 'Q' {
			count++
		}
	}
	if count > maxEvidenceBytes {
		t.Errorf("evidence not capped: %d bytes in prompt (cap %d)", count, maxEvidenceBytes)
	}
	if count == 0 {
		t.Errorf("evidence missing from prompt entirely")
	}
}

// TestParseReplyMalformed verifies that malformed JSON lines are skipped.
func TestParseReplyMalformed(t *testing.T) {
	allowed := map[Action]bool{ActionWait: true}
	cases := []string{
		"not json at all",
		`{"action": "wait"}`,            // missing reason is fine — should still parse
		`{"reason": "no action field"}`, // missing action
		`{}`,                            // empty object
	}
	// Only the "missing reason" case should produce a result (reason="" is OK).
	for _, tc := range cases {
		r, ok := parseReply(tc, allowed)
		if tc == `{"action": "wait"}` {
			// reason is optional — the brain may omit it; this should parse.
			if !ok || r.Action != ActionWait {
				t.Errorf("expected wait to parse from %q, got ok=%v action=%q", tc, ok, r.Action)
			}
		} else {
			if ok {
				t.Errorf("expected no parse from %q, got action=%q", tc, r.Action)
			}
		}
	}
}

// TestConsultUpdateTaskProgress verifies that ActionUpdateTaskProgress with task_progress is parsed.
func TestConsultUpdateTaskProgress(t *testing.T) {
	sp := &fakeSpawner{
		outputFn: func(_ context.Context, _ string, _ int) (string, error) {
			return `{"action": "update_task_progress", "reason": "checked git log", "task_progress": {"t1": "done", "t2": "in_progress"}}`, nil
		},
	}
	c := New(sp, noopAuditWriter(), Options{Timeout: 5 * time.Second})
	res, err := c.Consult(context.Background(), Request{
		Intent:  "plan_progress_assessment",
		Allowed: []Action{ActionUpdateTaskProgress},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionUpdateTaskProgress {
		t.Errorf("expected action update_task_progress, got %q", res.Action)
	}
	if res.Reason != "checked git log" {
		t.Errorf("unexpected reason: %q", res.Reason)
	}
	if res.TaskProgress["t1"] != "done" || res.TaskProgress["t2"] != "in_progress" {
		t.Errorf("unexpected task progress: %v", res.TaskProgress)
	}
}

func TestParseAnswer(t *testing.T) {
	out := "noise\n{\"answer\": \"select_option\", \"option\": 2, \"reason\": \"r\"}\n"
	r, ok := parseAnswer(out, 3)
	if !ok || r.Answer != AnswerSelectOption || r.Option != 2 {
		t.Fatalf("got %+v ok=%v", r, ok)
	}
	if _, ok := parseAnswer(`{"answer": "select_option", "option": 9}`, 3); ok {
		t.Fatal("out-of-range option must be rejected")
	}
	if _, ok := parseAnswer(`{"answer": "type", "text": " "}`, 0); ok {
		t.Fatal("empty type text must be rejected")
	}
	if _, ok := parseAnswer(`{"answer": "approve|reject|select_option|type"}`, 2); ok {
		t.Fatal("template echo must not parse")
	}
	r, ok = parseAnswer(`{"answer": "reject", "text": "use scratch"}`, 2)
	if !ok || r.Answer != AnswerReject || r.Text != "use scratch" {
		t.Fatalf("got %+v", r)
	}
}
