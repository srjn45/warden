package poller

import (
	"context"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestCrashAnomaly_OOMSignature(t *testing.T) {
	a, ok := crashAnomaly(137)
	require.True(t, ok, "exit 137 (128+SIGKILL) must be flagged as a possible OOM kill")
	require.Equal(t, anomalyOOM, a.Kind)
	require.Contains(t, a.Detail, "OOM")
}

func TestCrashAnomaly_OrdinaryCrashHasNoExtraSignal(t *testing.T) {
	// A plain non-zero exit is already covered by FinalizeExit's exit event — the
	// poller raises no extra anomaly for it.
	for _, code := range []int{1, 2, 130, 139} {
		_, ok := crashAnomaly(code)
		require.False(t, ok, "exit %d must not be classified as an OOM kill", code)
	}
}

func TestTickRaisesOOMAnomalyOnSIGKILLExit(t *testing.T) {
	d := &stubDeps{
		sessions:  []*store.Session{{ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking}},
		alive:     map[string]bool{"A-1": false},
		panes:     map[string]string{},
		updates:   map[string]store.Status{},
		exitCodes: map[string]int{"A-1": 137},
	}
	var gotAnomaly *Anomaly
	p := New(d, 5*time.Minute)
	p.OnAnomaly = func(_ *store.Session, a Anomaly) { gotAnomaly = &a }
	require.NoError(t, p.tick(context.Background()))

	require.Equal(t, store.StatusErrored, d.finalized["A-1"], "SIGKILL exit code finalizes as errored")
	evs := d.recordedEvents("A-1")
	require.Len(t, evs, 1, "an OOM-suspected crash records exactly one anomaly event")
	require.Equal(t, "anomaly", evs[0].Type)
	require.Contains(t, evs[0].Detail, "OOM")
	require.NotNil(t, gotAnomaly, "OnAnomaly must fire for an OOM-suspected crash")
	require.Equal(t, anomalyOOM, gotAnomaly.Kind)
}

func TestTickNoAnomalyOnCleanOrOrdinaryExit(t *testing.T) {
	for _, code := range []int{0, 1} {
		d := &stubDeps{
			sessions:  []*store.Session{{ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking}},
			alive:     map[string]bool{"A-1": false},
			panes:     map[string]string{},
			updates:   map[string]store.Status{},
			exitCodes: map[string]int{"A-1": code},
		}
		p := New(d, 5*time.Minute)
		require.NoError(t, p.tick(context.Background()))
		require.Empty(t, d.recordedEvents("A-1"), "exit %d must not raise an anomaly event", code)
	}
}
