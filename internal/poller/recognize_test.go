package poller

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentbackend/backends"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// unknownPane is a permission menu in a wording no backend parser knows.
const unknownPane = `● Shell(make build)

Tool request
──────────────────────────────

Wants to execute:
   make build

Shall I go ahead?
› Go ahead once
  Go ahead and always allow make
  Refuse

  arrows move · return picks
`

const unknownReply = `{"is_prompt":true,"question":"Shall I go ahead?","action":"make build",
 "options":["Go ahead once","Go ahead and always allow make","Refuse"],
 "affirmative":1,"sticky":false,"kind":"permission","confidence":0.93,"rationale":"permission menu"}`

// blindBackend recognizes nothing — every CLI after its vendor reworded a prompt.
type blindBackend struct{ backends.Claude }

func (blindBackend) ParseApproval(string) (*agentbackend.Approval, bool) { return nil, false }
func (blindBackend) DetectState(string) agentbackend.State               { return agentbackend.StateWorking }

// menuDeps is stubDeps with a live-ish cursor menu: Down/Up move the › cursor
// and Enter dismisses the menu.
type menuDeps struct{ *stubDeps }

func (d *menuDeps) SendKeys(ctx context.Context, tmuxSession, key string) error {
	_ = d.stubDeps.SendKeys(ctx, tmuxSession, key)
	if key == "Enter" {
		d.panes[tmuxSession] = "● Shell(make build)\n  ok\n"
	}
	return nil
}

func newRecogPoller(t *testing.T, d Deps, reply string) (*Poller, *atomic.Int32) {
	t.Helper()
	oldAfter, oldVerify := recognizeAfter, answerVerifyDelay
	recognizeAfter, answerVerifyDelay = 0, 0
	t.Cleanup(func() { recognizeAfter, answerVerifyDelay = oldAfter, oldVerify })
	calls := &atomic.Int32{}
	r := fastbrain.RunnerFunc(func(context.Context, string) (string, error) {
		calls.Add(1)
		return reply, nil
	})
	p := New(d, 30*time.Second)
	p.Backend = func(*agentstore.Agent) agentbackend.Backend { return blindBackend{} }
	p.FastBrain = fastbrain.NewEngine(r, r)
	p.SetRecognizePrompts(true)
	return p, calls
}

// TestRecognizeUnknownPromptIsAutoApproved is the headline: a prompt no parser
// knows is read by the model, verified, and answered through the normal
// auto-approve path with a cursor-verified Enter.
func TestRecognizeUnknownPromptIsAutoApproved(t *testing.T) {
	d := &menuDeps{&stubDeps{panes: map[string]string{"tmux-1": unknownPane}}}
	p, calls := newRecogPoller(t, d, unknownReply)
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1", AutoApprove: true}
	ctx := context.Background()

	p.tryRecognize(ctx, s, unknownPane)
	p.recogWG.Wait()
	require.EqualValues(t, 1, calls.Load())

	ap, ok := p.ParseApproval(s, unknownPane)
	require.True(t, ok)
	require.True(t, ap.Inferred)
	require.True(t, ap.Navigate)
	require.Equal(t, "Shall I go ahead?", ap.Question)
	require.Equal(t, "make build", ap.Action)
	require.Equal(t, 1, ap.SelectedIdx)
	require.Equal(t, 1, ap.AffirmativeIdx)
	require.False(t, ap.AffirmativeSticky)
	require.True(t, p.recognizedLive(s, unknownPane))

	evs := d.recordedEvents("agent-1")
	require.Len(t, evs, 1)
	require.Equal(t, "prompt_recognized", evs[0].Type)

	// The recognition published an approval event; run what the worker would.
	select {
	case ev := <-p.ApprovalEvents:
		p.tryAutoApprove(ctx, ev.Agent, ev.Pane)
	default:
		t.Fatal("no approval event published")
	}
	require.Equal(t, []string{"Enter"}, d.sentSequence("tmux-1"))

	// The menu is gone: the reading no longer applies, and asking again about the
	// same agent costs nothing.
	_, ok = p.ParseApproval(s, d.panes["tmux-1"])
	require.False(t, ok)
	p.tryRecognize(ctx, s, d.panes["tmux-1"])
	p.recogWG.Wait()
	require.EqualValues(t, 1, calls.Load())
}

// TestRecognizeAsksOncePerMenu: the same stalled menu is not re-sent to the model
// every tick, and a menu must sit for recognizeAfter before it is asked about.
func TestRecognizeAsksOncePerMenu(t *testing.T) {
	d := &menuDeps{&stubDeps{panes: map[string]string{"tmux-1": unknownPane}}}
	p, calls := newRecogPoller(t, d, `{"is_prompt":false,"confidence":0.9}`)
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1", AutoApprove: true}
	ctx := context.Background()

	recognizeAfter = time.Hour
	p.tryRecognize(ctx, s, unknownPane)
	p.recogWG.Wait()
	require.Zero(t, calls.Load(), "a menu that just appeared is not asked about yet")

	recognizeAfter = 0
	for i := 0; i < 6; i++ {
		p.tryRecognize(ctx, s, unknownPane)
		p.recogWG.Wait()
	}
	require.EqualValues(t, recognizeMaxAttempts, calls.Load())
	_, ok := p.ParseApproval(s, unknownPane)
	require.False(t, ok, "the model said it is not a prompt")
	require.Empty(t, d.recordedEvents("agent-1"))
}

// TestRecognizeDiscardsUnverifiableReading: options the model paraphrased or
// invented are not on screen, so the reading is dropped and nothing is answered.
func TestRecognizeDiscardsUnverifiableReading(t *testing.T) {
	d := &menuDeps{&stubDeps{panes: map[string]string{"tmux-1": unknownPane}}}
	reply := strings.Replace(unknownReply, `"Go ahead once"`, `"Yes"`, 1)
	p, _ := newRecogPoller(t, d, reply)
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1", AutoApprove: true}

	p.tryRecognize(context.Background(), s, unknownPane)
	p.recogWG.Wait()

	_, ok := p.ParseApproval(s, unknownPane)
	require.False(t, ok)
	require.Zero(t, d.sendCount())
}

// TestRecognizeOffOrParsed: with the setting off, or when a parser already
// recognizes the pane, the model is never called.
func TestRecognizeOffOrParsed(t *testing.T) {
	d := &menuDeps{&stubDeps{panes: map[string]string{"tmux-1": unknownPane}}}
	p, calls := newRecogPoller(t, d, unknownReply)
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1"}
	ctx := context.Background()

	p.SetRecognizePrompts(false)
	p.tryRecognize(ctx, s, unknownPane)
	p.recogWG.Wait()
	require.Zero(t, calls.Load())

	p.SetRecognizePrompts(true)
	p.Backend = func(*agentstore.Agent) agentbackend.Backend { return parsingBackend{} }
	p.tryRecognize(ctx, s, unknownPane)
	p.recogWG.Wait()
	require.Zero(t, calls.Load())

	p.Backend = func(*agentstore.Agent) agentbackend.Backend { return blindBackend{} }
	p.tryRecognize(ctx, s, "● working on it\n  compiling...\n")
	p.recogWG.Wait()
	require.Zero(t, calls.Load(), "a pane without a menu never reaches the model")
}

type parsingBackend struct{ backends.Claude }

func (parsingBackend) ParseApproval(string) (*agentbackend.Approval, bool) {
	return &agentbackend.Approval{Options: []string{"Yes", "No"}, AffirmativeIdx: 1}, true
}

// TestRecognizedDestructiveIsNeverAnswered: the model summarized a dangerous
// command away, but the guard reads the pane block, not the model's summary.
func TestRecognizedDestructiveIsNeverAnswered(t *testing.T) {
	pane := strings.ReplaceAll(unknownPane, "make build", "rm -rf /var/data")
	reply := strings.Replace(unknownReply, `"action":"make build"`, `"action":""`, 1)
	d := &menuDeps{&stubDeps{panes: map[string]string{"tmux-1": pane}}}
	p, _ := newRecogPoller(t, d, reply)
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1", AutoApprove: true}
	ctx := context.Background()

	p.tryRecognize(ctx, s, pane)
	p.recogWG.Wait()
	ap, ok := p.ParseApproval(s, pane)
	require.True(t, ok)
	bad, _ := approval.IsDestructive(approval.Approval{Action: ap.Action, Question: ap.Question})
	require.True(t, bad, "the pane block carries the command to the destructive guard")

	p.tryAutoApprove(ctx, s, pane)
	require.Zero(t, d.sendCount(), "a destructive prompt is never auto-answered")
}

// TestRecognizedApprovalDoesNotTrustTheModel covers the deterministic clamps on
// a model's reading.
func TestRecognizedApprovalDoesNotTrustTheModel(t *testing.T) {
	opts := []string{"Go ahead once", "Go ahead and always allow make", "Refuse"}

	// "affirmative" pointing at a refusal is dropped.
	ap := recognizedApproval(unknownPane, fastbrain.Recognition{Options: opts, Affirmative: 3})
	require.NotNil(t, ap)
	require.Zero(t, ap.AffirmativeIdx)

	// A standing grant is sticky whatever the model claimed.
	ap = recognizedApproval(unknownPane, fastbrain.Recognition{Options: opts, Affirmative: 2, Sticky: false})
	require.True(t, ap.AffirmativeSticky)

	// A question or action that is not on screen is not kept.
	ap = recognizedApproval(unknownPane, fastbrain.Recognition{Options: opts, Affirmative: 1,
		Question: "Is this safe?", Action: "echo harmless"})
	require.Equal(t, "Shall I go ahead?", ap.Question, "falls back to the line above the options")
	require.Empty(t, ap.Action)

	// "trust" is honoured only when the pane itself talks about trust.
	ap = recognizedApproval(unknownPane, fastbrain.Recognition{Options: opts, Affirmative: 1, Trust: true})
	require.Empty(t, ap.Kind)
	trustPane := "Opening /home/dev/project\n\nIs this a folder you trust?\n› Yes, continue\n  No, leave\n"
	ap = recognizedApproval(trustPane, fastbrain.Recognition{Options: []string{"Yes, continue", "No, leave"}, Affirmative: 1, Trust: true})
	require.Equal(t, agentbackend.ApprovalKindTrust, ap.Kind)
	require.True(t, ap.AffirmativeSticky)
}

// TestRecognizedPromptReportsWaitingForInput: the backend's state detection
// calls the pane "working", but a tick reports waiting_for_input once the model
// has read the prompt.
func TestRecognizedPromptReportsWaitingForInput(t *testing.T) {
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1", Status: store.StatusWorking, UpdatedAt: time.Now()}
	d := &menuDeps{&stubDeps{
		sessions: []*agentstore.Agent{s},
		alive:    map[string]bool{"tmux-1": true},
		panes:    map[string]string{"tmux-1": unknownPane},
		updates:  map[string]store.Status{},
	}}
	p, _ := newRecogPoller(t, d, unknownReply)
	ctx := context.Background()

	require.NoError(t, p.tick(ctx))
	p.recogWG.Wait()
	require.NoError(t, p.tick(ctx))
	require.Equal(t, store.StatusWaitingForInput, d.updates["agent-1"])
}
