package poller

import (
	"context"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/knownprompts"
	"github.com/stretchr/testify/require"
)

// clearedPane is what menuDeps leaves behind once Enter dismisses the menu.
const clearedPane = "● Shell(make build)\n  ok\n"

func newLearnPoller(t *testing.T, d Deps) (*Poller, *knownprompts.Store, string, func() int32) {
	t.Helper()
	p, calls := newRecogPoller(t, d, unknownReply)
	old := learnVerifyAfter
	learnVerifyAfter = 0
	t.Cleanup(func() { learnVerifyAfter = old })
	dir := t.TempDir()
	ks, err := knownprompts.New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ks.Close() })
	p.Known = ks
	return p, ks, dir, calls.Load
}

func eventTypes(d *menuDeps, id string) []string {
	var out []string
	for _, e := range d.recordedEvents(id) {
		out = append(out, e.Type)
	}
	return out
}

func recognizeAndAnswer(t *testing.T, p *Poller, s *agentstore.Agent, pane string) {
	t.Helper()
	ctx := context.Background()
	p.tryRecognize(ctx, s, pane)
	p.recogWG.Wait()
	p.tryAutoApprove(ctx, s, pane)
}

func TestLearnAfterClear(t *testing.T) {
	d := &menuDeps{&stubDeps{panes: map[string]string{"tmux-1": unknownPane}}}
	p, ks, _, calls := newLearnPoller(t, d)
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1", AutoApprove: true}
	ctx := context.Background()

	recognizeAndAnswer(t, p, s, unknownPane)
	require.Equal(t, []string{"Enter"}, d.sentSequence("tmux-1"))
	require.Empty(t, ks.List(), "nothing is learned before the clear is seen")

	p.checkLearn(ctx, s, d.panes["tmux-1"]) // menu is gone
	p.recogWG.Wait()
	require.Len(t, ks.List(), 1)
	require.Contains(t, eventTypes(d, "agent-1"), "prompt_learned")

	// The next occurrence (another agent) is read from the store, no model.
	pane2 := unknownPane
	s2 := &agentstore.Agent{ID: "agent-2", TmuxSession: "tmux-2"}
	p.tryRecognize(ctx, s2, pane2)
	p.recogWG.Wait()
	ap, ok := p.ParseApproval(s2, pane2)
	require.True(t, ok)
	require.True(t, ap.Inferred)
	require.EqualValues(t, 1, calls(), "only the first occurrence cost a model call")
}

func TestNoLearnWhenStillShowing(t *testing.T) {
	d := &menuDeps{&stubDeps{panes: map[string]string{"tmux-1": unknownPane}}}
	p, ks, _, _ := newLearnPoller(t, d)
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1", AutoApprove: true}

	recognizeAndAnswer(t, p, s, unknownPane)
	p.checkLearn(context.Background(), s, unknownPane) // menu survived the answer
	p.recogWG.Wait()
	require.Empty(t, ks.List())
	require.NotContains(t, eventTypes(d, "agent-1"), "prompt_learned")
}

func TestNoLearnOnPromptChanged(t *testing.T) {
	d := &menuDeps{&stubDeps{panes: map[string]string{"tmux-1": unknownPane}}}
	p, ks, _, _ := newLearnPoller(t, d)
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1", AutoApprove: true}
	ctx := context.Background()

	p.tryRecognize(ctx, s, unknownPane)
	p.recogWG.Wait()
	// The cursor moves under the answer, so Enter is never sent.
	d.panes["tmux-1"] = strings.Replace(strings.Replace(unknownPane, "› Go ahead once", "  Go ahead once", 1), "  Refuse", "› Refuse", 1)
	p.tryAutoApprove(ctx, s, unknownPane)
	require.Zero(t, d.sendCount())

	p.checkLearn(ctx, s, clearedPane)
	p.recogWG.Wait()
	require.Empty(t, ks.List())
}

func TestHumanAnsweredPromptIsLearned(t *testing.T) {
	d := &menuDeps{&stubDeps{panes: map[string]string{"tmux-1": unknownPane}}}
	p, ks, _, _ := newLearnPoller(t, d)
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1"} // no auto-approve
	ctx := context.Background()

	p.tryRecognize(ctx, s, unknownPane)
	p.recogWG.Wait()
	ap, ok := p.ParseApproval(s, unknownPane)
	require.True(t, ok)
	// What the approve endpoint does after agentbackend.Answer succeeds.
	p.NoteAnswered(s, ap, unknownPane)
	p.checkLearn(ctx, s, clearedPane)
	p.recogWG.Wait()
	require.Len(t, ks.List(), 1)
}

func TestParserReadPromptIsNeverLearned(t *testing.T) {
	d := &menuDeps{&stubDeps{}}
	p, ks, _, _ := newLearnPoller(t, d)
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1"}
	ap := &agentbackend.Approval{
		Question: "Shall I go ahead?", Options: []string{"Go ahead once", "Go ahead and always allow make", "Refuse"},
		SelectedIdx: 1, AffirmativeIdx: 1, Navigate: true, // Inferred=false: a parser read it
	}
	p.NoteAnswered(s, ap, unknownPane)
	p.checkLearn(context.Background(), s, clearedPane)
	p.recogWG.Wait()
	require.Empty(t, ks.List())
}

func TestInvalidateOnBreakerThenModelFallback(t *testing.T) {
	pane := knownPane("make lint")
	d := &menuDeps{&stubDeps{panes: map[string]string{"tmux-1": pane}}}
	p, ks, _, calls := newLearnPoller(t, d)
	_, _, err := ks.Learn(context.Background(), "claude", "", knownprompts.Reading{
		Question: "Shall I go ahead?", Action: "make build",
		Options:     []string{"Go ahead once", "Go ahead and always allow make build", "Refuse"},
		Affirmative: 1,
	})
	require.NoError(t, err)
	p.AutoApprovePolicy = approval.Policy{Enabled: true, MaxRepeats: 1}
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1", AutoApprove: true}
	ctx := context.Background()

	p.tryRecognize(ctx, s, pane)
	p.tryAutoApprove(ctx, s, pane) // answered once
	require.Len(t, ks.List(), 1)
	p.tryAutoApprove(ctx, s, pane) // breaker trips on the store-read prompt
	require.Empty(t, ks.List())
	require.Contains(t, eventTypes(d, "agent-1"), "prompt_known_invalidated")
	require.Zero(t, calls())

	// The next occurrence goes to the model.
	d.panes["tmux-1"] = pane
	p.tryRecognize(ctx, s, pane)
	p.recogWG.Wait()
	require.EqualValues(t, 1, calls())
}

func TestInvalidateAfterRepeatedUncleared(t *testing.T) {
	pane := knownPane("make lint")
	d := &menuDeps{&stubDeps{panes: map[string]string{"tmux-1": pane}}}
	p, ks, _, calls := newLearnPoller(t, d)
	_, _, err := ks.Learn(context.Background(), "claude", "", knownprompts.Reading{
		Question: "Shall I go ahead?", Action: "make build",
		Options:     []string{"Go ahead once", "Go ahead and always allow make build", "Refuse"},
		Affirmative: 1,
	})
	require.NoError(t, err)
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1", AutoApprove: true}
	ctx := context.Background()

	for i := 0; i < knownMaxStrikes; i++ {
		require.Len(t, ks.List(), 1, "round %d", i)
		d.panes["tmux-1"] = pane // the menu survives the answer every time
		p.tryRecognize(ctx, s, pane)
		p.tryAutoApprove(ctx, s, pane)
		d.panes["tmux-1"] = pane
		p.checkLearn(ctx, s, pane)
	}
	require.Empty(t, ks.List())
	require.Contains(t, eventTypes(d, "agent-1"), "prompt_known_invalidated")
	require.Zero(t, calls())
}

func TestKnownConfirmedResetsStrikes(t *testing.T) {
	pane := knownPane("make lint")
	d := &menuDeps{&stubDeps{panes: map[string]string{"tmux-1": pane}}}
	p, ks, _, _ := newLearnPoller(t, d)
	_, _, err := ks.Learn(context.Background(), "claude", "", knownprompts.Reading{
		Question: "Shall I go ahead?", Action: "make build",
		Options:     []string{"Go ahead once", "Go ahead and always allow make build", "Refuse"},
		Affirmative: 1,
	})
	require.NoError(t, err)
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1", AutoApprove: true}
	ctx := context.Background()
	id := ks.List()[0].ID

	p.knownStrikes = map[string]int{id: knownMaxStrikes - 1}
	p.tryRecognize(ctx, s, pane)
	p.tryAutoApprove(ctx, s, pane)
	p.checkLearn(ctx, s, d.panes["tmux-1"]) // cleared
	require.Zero(t, p.knownStrikes[id])
	require.Len(t, ks.List(), 1)
}

func TestLearnedPromptSurvivesRestart(t *testing.T) {
	d := &menuDeps{&stubDeps{panes: map[string]string{"tmux-1": unknownPane}}}
	p, ks, dir, _ := newLearnPoller(t, d)
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1", AutoApprove: true}
	recognizeAndAnswer(t, p, s, unknownPane)
	p.checkLearn(context.Background(), s, d.panes["tmux-1"])
	p.recogWG.Wait()
	require.Len(t, ks.List(), 1)
	require.NoError(t, ks.Close())

	ks2, err := knownprompts.New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ks2.Close() })
	pane := unknownPane
	d2 := &menuDeps{&stubDeps{panes: map[string]string{"tmux-9": pane}}}
	p2, calls := newRecogPoller(t, d2, unknownReply)
	p2.Known = ks2
	s2 := &agentstore.Agent{ID: "agent-9", TmuxSession: "tmux-9"}
	p2.tryRecognize(context.Background(), s2, pane)
	p2.recogWG.Wait()
	ap, ok := p2.ParseApproval(s2, pane)
	require.True(t, ok)
	require.True(t, ap.Inferred)
	require.Zero(t, calls.Load(), "a persisted entry needs no model call")
}
