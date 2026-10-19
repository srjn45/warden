package ptyruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/ptyevent"
)

type mirrorRig struct {
	t   *testing.T
	dir string
	j   *Journal
	m   *Mirror
	w   *os.File
}

func startMirror(t *testing.T, dir string, recover bool, seed string) *mirrorRig {
	t.Helper()
	j, err := OpenJournal(dir, "sess-1", Retention{})
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	m, err := StartMirror(MirrorConfig{Journal: j, Source: r, Recover: recover, Seed: seed, Cols: 80, Rows: 24,
		Options: Options{IdleFlush: 20 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	return &mirrorRig{t: t, dir: dir, j: j, m: m, w: w}
}

// stop ends the source (EOF), optionally journals an exit, and tears down.
func (r *mirrorRig) stop(exit *ptyevent.Lifecycle) {
	r.t.Helper()
	_ = r.w.Close()
	select {
	case <-r.m.Done():
	case <-time.After(waitFor):
		r.t.Fatal("mirror did not drain")
	}
	if exit != nil {
		if err := r.m.Exit(*exit); err != nil {
			r.t.Fatal(err)
		}
	}
	_ = r.m.Close()
	_ = r.j.Close()
}

func (r *mirrorRig) waitOutput(want string) {
	r.t.Helper()
	deadline := time.Now().Add(waitFor)
	for time.Now().Before(deadline) {
		var b strings.Builder
		_ = r.j.Read(1, func(ev ptyevent.Event) error {
			if ev.Kind == ptyevent.KindOutput {
				b.Write(ev.Output.Data)
			}
			return nil
		})
		if strings.Contains(b.String(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.t.Fatalf("journal never showed %q", want)
}

func TestMirrorJournalsRedactedPaneOutput(t *testing.T) {
	noLeaks(t)
	dir := filepath.Join(t.TempDir(), "j")
	r := startMirror(t, dir, false, "")
	secret := fakeSecret()
	// The secret is split across two writes: redaction must hold it together.
	_, _ = r.w.WriteString("hello from tmux\nkey=" + secret[:10])
	_, _ = r.w.WriteString(secret[10:] + "\nbye\n")
	r.waitOutput("bye")
	sub := r.m.Subscribe(ptyevent.Cursor{})
	r.stop(&ptyevent.Lifecycle{Phase: ptyevent.PhaseExited, Detail: "tmux session ended"})
	var replay []ptyevent.Event
	for ev := range sub.C {
		replay = append(replay, ev)
	}

	evs := readAll(t, dir)
	if vs := ptyevent.ValidateStream(evs); len(vs) > 0 {
		t.Fatalf("stream violations: %v", vs)
	}
	if evs[0].Kind != ptyevent.KindLifecycle || evs[0].Lifecycle.Phase != ptyevent.PhaseSpawned ||
		evs[0].Lifecycle.Presentation != ptyevent.PresentationTmux {
		t.Fatalf("first event = %+v, want lifecycle{spawned, tmux}", evs[0])
	}
	last := evs[len(evs)-1]
	if last.Kind != ptyevent.KindLifecycle || last.Lifecycle.Phase != ptyevent.PhaseExited {
		t.Fatalf("last event = %+v, want lifecycle{exited}", last)
	}
	if disk := readDir(t, dir); strings.Contains(disk, secret) {
		t.Fatal("unredacted secret reached the journal")
	}
	for _, ev := range replay {
		if ev.Kind == ptyevent.KindOutput && strings.Contains(string(ev.Output.Data), secret) {
			t.Fatal("unredacted secret reached a subscriber")
		}
	}
	var out strings.Builder
	for _, ev := range evs {
		if ev.Kind == ptyevent.KindOutput {
			out.Write(ev.Output.Data)
		}
	}
	if !strings.Contains(out.String(), "[REDACTED:") || strings.Contains(out.String(), secret) {
		t.Fatalf("journaled output must carry the placeholder, not the secret: %q", out.String())
	}
}

func TestMirrorRecoverOpensNewEpochWithSeededSnapshot(t *testing.T) {
	noLeaks(t)
	dir := filepath.Join(t.TempDir(), "j")
	r := startMirror(t, dir, false, "")
	_, _ = r.w.WriteString("before restart\n")
	r.waitOutput("before restart")
	r.stop(nil) // daemon shutdown: the pane lives on, epoch stays open

	// Re-adoption after the restart, seeded from the live screen.
	r2 := startMirror(t, dir, true, "$ before restart\n$ ")
	if r2.m.Epoch() != 2 {
		t.Fatalf("epoch = %d, want 2", r2.m.Epoch())
	}
	_, _ = r2.w.WriteString("after restart\n")
	r2.waitOutput("after restart")
	r2.stop(&ptyevent.Lifecycle{Phase: ptyevent.PhaseExited})

	evs := readAll(t, dir)
	if vs := ptyevent.ValidateStream(evs); len(vs) > 0 {
		t.Fatalf("stream violations: %v", vs)
	}
	var phases []ptyevent.LifecyclePhase
	var sawReset, sawSnap bool
	for _, ev := range evs {
		switch {
		case ev.Kind == ptyevent.KindLifecycle:
			phases = append(phases, ev.Lifecycle.Phase)
		case ev.Kind == ptyevent.KindMarker && ev.Marker.Type == ptyevent.MarkerReset:
			sawReset = true
		case ev.Kind == ptyevent.KindMarker && ev.Marker.Type == ptyevent.MarkerSnapshot && ev.Epoch == 2:
			sawSnap = strings.Contains(ev.Marker.Snapshot, "before restart")
		}
	}
	want := []ptyevent.LifecyclePhase{ptyevent.PhaseSpawned, ptyevent.PhaseRecovered, ptyevent.PhaseExited}
	if strings.Join(phaseStrings(phases), ",") != strings.Join(phaseStrings(want), ",") {
		t.Fatalf("lifecycle phases = %v, want %v (no crashed: the pane was alive)", phases, want)
	}
	if !sawReset || !sawSnap {
		t.Fatalf("recover must journal reset (%v) and a seeded snapshot (%v)", sawReset, sawSnap)
	}
	// A client holding a pre-restart cursor is resynchronized from the snapshot.
	j, _ := OpenJournal(dir, "sess-1", Retention{})
	defer j.Close()
	if plan := ptyevent.PlanResume(ptyevent.Cursor{SessionID: "sess-1", Epoch: 1, Seq: 3}, j.Window()); plan.Mode != ptyevent.ResumeSnapshot {
		t.Fatalf("old cursor plan = %+v, want snapshot", plan)
	}
}

func TestMirrorUnrecoveredRestartJournalsCrash(t *testing.T) {
	noLeaks(t)
	dir := filepath.Join(t.TempDir(), "j")
	r := startMirror(t, dir, false, "")
	_, _ = r.w.WriteString("x\n")
	r.waitOutput("x")
	r.stop(nil)
	// A new pane under the same name (the old one died unobserved).
	r2 := startMirror(t, dir, false, "")
	r2.stop(nil)
	evs := readAll(t, dir)
	if vs := ptyevent.ValidateStream(evs); len(vs) > 0 {
		t.Fatalf("stream violations: %v", vs)
	}
	var crashed bool
	for _, ev := range evs {
		crashed = crashed || (ev.Kind == ptyevent.KindLifecycle && ev.Lifecycle.Phase == ptyevent.PhaseCrashed && ev.Epoch == 1)
	}
	if !crashed {
		t.Fatal("an unended epoch replaced by a new pane must be journaled as crashed")
	}
}

func TestMirrorDropsOutputAfterExit(t *testing.T) {
	noLeaks(t)
	dir := filepath.Join(t.TempDir(), "j")
	r := startMirror(t, dir, false, "")
	if err := r.m.Exit(ptyevent.Lifecycle{Phase: ptyevent.PhaseExited}); err != nil {
		t.Fatal(err)
	}
	if err := r.m.Exit(ptyevent.Lifecycle{Phase: ptyevent.PhaseSpawned}); err == nil {
		t.Fatal("Exit must reject a non-terminal phase")
	}
	_, _ = r.w.WriteString("late output\n")
	r.stop(nil)
	evs := readAll(t, dir)
	if vs := ptyevent.ValidateStream(evs); len(vs) > 0 {
		t.Fatalf("stream violations: %v", vs)
	}
	for _, ev := range evs {
		if ev.Kind == ptyevent.KindOutput {
			t.Fatalf("output journaled after exit: %q", ev.Output.Data)
		}
	}
}

func phaseStrings(ps []ptyevent.LifecyclePhase) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = string(p)
	}
	return out
}
