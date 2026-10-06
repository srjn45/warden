package poller

import (
	"context"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/srjn45/warden/internal/knownprompts"
	"github.com/stretchr/testify/require"
)

// newKnownPoller is newRecogPoller with a known-prompts store that has learned
// unknownPane (with `make build` templated out of the labels).
func newKnownPoller(t *testing.T, d Deps, affirmative int, sticky []bool) (*Poller, *knownprompts.Store, func() int32) {
	t.Helper()
	p, calls := newRecogPoller(t, d, unknownReply)
	ks, err := knownprompts.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ks.Close() })
	_, _, err = ks.Learn(context.Background(), "claude", "", knownprompts.Reading{
		Question:    "Shall I go ahead?",
		Action:      "make build",
		Options:     []string{"Go ahead once", "Go ahead and always allow make build", "Refuse"},
		Affirmative: affirmative,
		Sticky:      sticky,
	})
	require.NoError(t, err)
	p.Known = ks
	return p, ks, calls.Load
}

// knownPane shows the learned shape with a different command in the labels.
func knownPane(cmd string) string {
	return strings.ReplaceAll(strings.ReplaceAll(unknownPane, "always allow make", "always allow "+cmd), "make build", cmd)
}

func TestKnownPromptHitNeedsNoModel(t *testing.T) {
	pane := knownPane("make lint")
	d := &menuDeps{&stubDeps{panes: map[string]string{"tmux-1": pane, "tmux-2": pane}}}
	p, ks, calls := newKnownPoller(t, d, 1, nil)
	ctx := context.Background()

	// The model path's reading of the same screen, for comparison.
	want := recognizedApproval(pane, fastbrainReading())

	for _, id := range []string{"agent-1", "agent-2"} { // different agents
		s := &agentstore.Agent{ID: id, TmuxSession: "tmux-" + id[len(id)-1:]}
		recognizeAfter = 1 << 60 // a store hit must not wait out the stall delay
		p.tryRecognize(ctx, s, pane)
		p.tryRecognize(ctx, s, pane) // once per menu
		p.recogWG.Wait()

		ap, ok := p.ParseApproval(s, pane)
		require.True(t, ok)
		require.Equal(t, want.Question, ap.Question)
		require.Equal(t, want.Options, ap.Options)
		require.Equal(t, want.SelectedIdx, ap.SelectedIdx)
		require.Equal(t, want.AffirmativeIdx, ap.AffirmativeIdx)
		require.Equal(t, want.AffirmativeSticky, ap.AffirmativeSticky)
		require.True(t, ap.Inferred)
		require.True(t, ap.Navigate)
		require.True(t, p.recognizedLive(s, pane))

		evs := d.recordedEvents(id)
		require.Len(t, evs, 1)
		require.Equal(t, "prompt_known", evs[0].Type)
	}
	require.Zero(t, calls(), "a store hit never calls the model")
	require.Equal(t, 2, ks.List()[0].Hits)
}

func TestKnownPromptDestructiveStillBlocked(t *testing.T) {
	pane := knownPane("rm -rf /var/data")
	d := &menuDeps{&stubDeps{panes: map[string]string{"tmux-1": pane}}}
	p, _, calls := newKnownPoller(t, d, 1, nil)
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1", AutoApprove: true}
	ctx := context.Background()

	p.tryRecognize(ctx, s, pane)
	p.recogWG.Wait()
	ap, ok := p.ParseApproval(s, pane)
	require.True(t, ok)
	bad, _ := approval.IsDestructive(approval.Approval{Action: ap.Action, Question: ap.Question})
	require.True(t, bad)
	p.tryAutoApprove(ctx, s, pane)
	require.Zero(t, d.sendCount())
	require.Zero(t, calls())
}

func TestKnownPromptStillGoesThroughPolicy(t *testing.T) {
	pane := knownPane("make lint")
	ctx := context.Background()

	// A normal affirmative is answered with a cursor-verified Enter.
	d := &menuDeps{&stubDeps{panes: map[string]string{"tmux-1": pane}}}
	p, _, _ := newKnownPoller(t, d, 1, nil)
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1", AutoApprove: true}
	p.tryRecognize(ctx, s, pane)
	p.tryAutoApprove(ctx, s, pane)
	require.Equal(t, []string{"Enter"}, d.sentSequence("tmux-1"))

	// A learned standing grant is sticky: allow_sticky=false leaves it alone.
	d = &menuDeps{&stubDeps{panes: map[string]string{"tmux-1": pane}}}
	p, _, _ = newKnownPoller(t, d, 2, []bool{false, true, false})
	p.AutoApprovePolicy = approval.Policy{Enabled: true}
	p.tryRecognize(ctx, s, pane)
	ap, ok := p.ParseApproval(s, pane)
	require.True(t, ok)
	require.True(t, ap.AffirmativeSticky)
	p.tryAutoApprove(ctx, s, pane)
	require.Zero(t, d.sendCount())

	// A deny rule still wins over a learned prompt.
	d = &menuDeps{&stubDeps{panes: map[string]string{"tmux-1": pane}}}
	p, _, _ = newKnownPoller(t, d, 1, nil)
	p.AutoApprovePolicy = approval.Policy{Enabled: true, Rules: approval.Rules{Deny: []approval.Rule{{Pattern: "make lint"}}}}
	p.tryRecognize(ctx, s, pane)
	p.tryAutoApprove(ctx, s, pane)
	require.Zero(t, d.sendCount())
}

func TestKnownPromptOffReadsNothing(t *testing.T) {
	pane := knownPane("make lint")
	d := &menuDeps{&stubDeps{panes: map[string]string{"tmux-1": pane}}}
	p, ks, calls := newKnownPoller(t, d, 1, nil)
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1"}
	p.SetRecognizePrompts(false)
	p.tryRecognize(context.Background(), s, pane)
	p.recogWG.Wait()
	_, ok := p.ParseApproval(s, pane)
	require.False(t, ok)
	require.Empty(t, d.recordedEvents("agent-1"))
	require.Zero(t, calls())
	require.Zero(t, ks.List()[0].Hits)

	// nil store: feature absent, model path unaffected.
	p.SetRecognizePrompts(true)
	p.Known = nil
	_, ok = p.ParseApproval(s, pane)
	require.False(t, ok)
}

func fastbrainReading() fastbrain.Recognition {
	return fastbrain.Recognition{
		Question:    "Shall I go ahead?",
		Options:     []string{"Go ahead once", "Go ahead and always allow make lint", "Refuse"},
		Affirmative: 1,
	}
}
