package poller

import (
	"context"
	"strconv"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestLooksLikeLoop(t *testing.T) {
	cases := []struct {
		name    string
		samples []string
		want    bool
	}{
		{"empty", nil, false},
		{
			name:    "short window not enough signal",
			samples: []string{"a", "b", "a", "b"},
			want:    false,
		},
		{
			name:    "linear progress never loops",
			samples: []string{"a", "b", "c", "d", "e", "f", "g", "h"},
			want:    false,
		},
		{
			name:    "single stray repeat is not a loop",
			samples: []string{"a", "b", "c", "d", "e", "a", "f", "g"},
			want:    false,
		},
		{
			name:    "two-state churn is a loop",
			samples: []string{"a", "b", "a", "b", "a", "b", "a", "b"},
			want:    true,
		},
		{
			name:    "three-state cycle is a loop",
			samples: []string{"x", "y", "z", "x", "y", "z", "x", "y"},
			want:    true,
		},
		{
			name:    "blank excerpts are ignored",
			samples: []string{"", "", "", "", "", "", "", ""},
			want:    false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, looksLikeLoop(c.samples))
		})
	}
}

// TestTickRaisesLoopAnomalyOnChurn drives the poller through a churning pane and
// asserts a single loop anomaly is raised, not one per tick.
func TestTickRaisesLoopAnomalyOnChurn(t *testing.T) {
	sess := &store.Session{ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking}
	d := &stubDeps{
		sessions:    []*store.Session{sess},
		alive:       map[string]bool{"A-1": true},
		panes:       map[string]string{},
		updates:     map[string]store.Status{},
		paneUpdates: map[string]string{},
	}
	p := New(d, 0) // stuckAfter=0 so the quiet-stuck path can't interfere
	var anomalies int
	p.OnAnomaly = func(_ *store.Session, a Anomaly) {
		if a.Kind == anomalyLoop {
			anomalies++
		}
	}

	// Alternate the pane between two states across many ticks. Each tick must see
	// a *changed* pane (the loop signal lives in the pane-change path), and the
	// stored excerpt is updated so the next change is detected.
	for i := 0; i < 12; i++ {
		state := "alpha\nworking on the thing"
		if i%2 == 1 {
			state = "beta\nworking on the thing"
		}
		d.panes["A-1"] = state
		require.NoError(t, p.tick(context.Background()))
		sess.LastPaneExcerpt = d.paneUpdates["A-1"] // mimic the store persisting the excerpt
	}

	require.Equal(t, 1, anomalies, "a churning pane must raise exactly one loop anomaly, not one per tick")
	evs := d.recordedEvents("A-1")
	require.Len(t, evs, 1)
	require.Contains(t, evs[0].Detail, "loop")
}

// TestTickNoLoopAnomalyOnProgress confirms a pane that keeps advancing through
// distinct output never trips the loop detector.
func TestTickNoLoopAnomalyOnProgress(t *testing.T) {
	sess := &store.Session{ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking}
	d := &stubDeps{
		sessions:    []*store.Session{sess},
		alive:       map[string]bool{"A-1": true},
		panes:       map[string]string{},
		updates:     map[string]store.Status{},
		paneUpdates: map[string]string{},
	}
	p := New(d, 0)
	p.OnAnomaly = func(_ *store.Session, _ Anomaly) { t.Fatal("progressing pane must not raise an anomaly") }

	for i := 0; i < 12; i++ {
		d.panes["A-1"] = "step " + strconv.Itoa(i)
		require.NoError(t, p.tick(context.Background()))
		sess.LastPaneExcerpt = d.paneUpdates["A-1"]
	}
	require.Empty(t, d.recordedEvents("A-1"))
}
