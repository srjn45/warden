package poller

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// heurFB runs the deterministic heuristic classifier (nil engine) so staging is real.
type heurFB struct {
	fastbrain.Engine
	err   error
	calls int
}

func (f *heurFB) DiagnoseFailure(ctx context.Context, in fastbrain.CrashInput) (fastbrain.CrashDiagnosis, error) {
	f.calls++
	if f.err != nil {
		return fastbrain.CrashDiagnosis{}, f.err
	}
	return fastbrain.DiagnoseFailure(ctx, nil, in)
}

func crashTick(t *testing.T, pane string, fb fastbrain.Engine) (*stubDeps, string) {
	d := &stubDeps{
		sessions:  []*agentstore.Agent{{ID: "A", Status: store.StatusWorking, LastPaneExcerpt: pane}},
		alive:     map[string]bool{"A": true},
		panes:     map[string]string{},
		updates:   map[string]store.Status{},
		exitCodes: map[string]int{"A": 2},
	}
	p := New(d, 5*time.Minute)
	p.FastBrain = fb
	p.CrashDir = t.TempDir()
	require.NoError(t, p.tick(context.Background()))
	p.triageWG.Wait()
	return d, p.CrashDir
}

func TestCrashTriageStagesInternalBugOnce(t *testing.T) {
	fb := &heurFB{}
	pane := "panic: runtime error: invalid memory address or nil pointer dereference\ngoroutine 1:\ngithub.com/srjn45/warden/internal/planstore.(*S).Update(0x0)"
	d, dir := crashTick(t, pane, fb)
	require.Equal(t, 1, fb.calls)
	drafts, err := fastbrain.ListCrashDrafts(dir)
	require.NoError(t, err)
	require.Len(t, drafts, 1)
	var found bool
	for _, ev := range d.recordedEvents("A") {
		if ev.Type == "bug_draft_staged" {
			found = true
			require.Contains(t, ev.Detail, "warden bug-report A")
		}
	}
	require.True(t, found, "durable event expected")
}

func TestCrashTriageSkipsOtherClasses(t *testing.T) {
	for name, pane := range map[string]string{
		"task_failure": "--- FAIL: TestX\nFAIL\texample.com/x 0.1s",
		"transient":    "Error: 429 Too Many Requests rate limit exceeded",
	} {
		t.Run(name, func(t *testing.T) {
			_, dir := crashTick(t, pane, &heurFB{})
			ents, _ := os.ReadDir(dir)
			require.Empty(t, ents)
		})
	}
}

func TestCrashTriageErrorDoesNotBreakTick(t *testing.T) {
	fb := &heurFB{err: errors.New("boom")}
	d, dir := crashTick(t, "panic: nil pointer", fb)
	require.Equal(t, store.StatusErrored, d.finalized["A"])
	ents, _ := os.ReadDir(dir)
	require.Empty(t, ents)
}
