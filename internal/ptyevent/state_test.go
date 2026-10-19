package ptyevent

import (
	"testing"
	"time"
)

type tick struct {
	ms   int
	ev   []Evidence
	want State
}

func ev(kinds ...EvidenceKind) []Evidence {
	out := make([]Evidence, len(kinds))
	for i, k := range kinds {
		out[i] = Evidence{Kind: k}
	}
	return out
}

func silence(d time.Duration, more ...Evidence) []Evidence {
	return append([]Evidence{{Kind: EvSilence, Quiet: d}}, more...)
}

func promptEv(p *Prompt) []Evidence { return []Evidence{{Kind: EvPrompt, Prompt: p}} }

func runTicks(t *testing.T, start Snapshot, ticks []tick) Snapshot {
	t.Helper()
	s := start
	for i, tk := range ticks {
		o := Observation{At: t0.Add(time.Duration(tk.ms) * time.Millisecond), Seq: uint64(i + 1), Evidence: tk.ev}
		var ch *StateChange
		s, ch = Step(s, o, DefaultHysteresis)
		if s.State != tk.want {
			t.Fatalf("tick %d (+%dms %v): state %s, want %s", i, tk.ms, kindsOf(tk.ev), s.State, tk.want)
		}
		if ch != nil {
			if len(ch.Evidence) == 0 {
				t.Fatalf("tick %d: transition without evidence", i)
			}
			for _, k := range ch.Evidence {
				if k == EvSilence && len(ch.Evidence) == 1 {
					t.Fatalf("tick %d: transition on silence alone", i)
				}
			}
		}
	}
	return s
}

func kindsOf(es []Evidence) []EvidenceKind {
	out := make([]EvidenceKind, len(es))
	for i, e := range es {
		out[i] = e.Kind
	}
	return out
}

func working() Snapshot { return Snapshot{State: StateWorking, Since: t0} }

func TestStateMachine(t *testing.T) {
	ps := fixturePrompts()
	cases := []struct {
		name  string
		start Snapshot
		ticks []tick
	}{
		{"spawn → startup → ready → idle", NewSnapshot(t0), []tick{
			{0, ev(EvProcessSpawn), StateWaiting},
			{100, ev(EvOutputActivity), StateWaiting},
			{500, ev(EvReady), StateIdle},
		}},
		{"silence alone never leaves working, however long", working(), []tick{
			{0, silence(10 * time.Second), StateWorking},
			{60_000, silence(time.Minute), StateWorking},
			{3_600_000, silence(time.Hour), StateWorking},
		}},
		{"output activity alone (a redraw) is not work", Snapshot{State: StateIdle, Since: t0}, []tick{
			{0, ev(EvOutputActivity), StateIdle},
			{100, ev(EvOutputActivity), StateIdle},
		}},
		{"input + output corroborate working", Snapshot{State: StateIdle, Since: t0}, []tick{
			{0, ev(EvInput, EvOutputActivity), StateWorking},
		}},
		{"bare idle marker is debounced (dwell + 2 hits)", working(), []tick{
			{0, ev(EvIdleMarker), StateWorking},
			{500, ev(EvIdleMarker), StateWorking},
			{1600, ev(EvIdleMarker), StateIdle},
		}},
		{"idle marker + silence corroborates immediately", working(), []tick{
			{0, silence(6*time.Second, Evidence{Kind: EvIdleMarker}), StateIdle},
		}},
		{"idle marker + transcript turn end corroborates", working(), []tick{
			{0, ev(EvIdleMarker, EvTurnEnd), StateIdle},
		}},
		{"hysteresis: a working marker between idle sightings resets the candidate", working(), []tick{
			{0, ev(EvIdleMarker), StateWorking},
			{1000, ev(EvWorkingMarker), StateWorking},
			{1700, ev(EvIdleMarker), StateWorking},
			{2000, ev(EvIdleMarker), StateWorking},
			{3300, ev(EvIdleMarker), StateIdle},
		}},
		{"silence between idle sightings neither commits nor resets", working(), []tick{
			{0, ev(EvIdleMarker), StateWorking},
			{800, silence(800 * time.Millisecond), StateWorking},
			{1600, ev(EvIdleMarker), StateIdle},
		}},
		{"stale pending candidate expires", working(), []tick{
			{0, ev(EvIdleMarker), StateWorking},
			{40_000, ev(EvIdleMarker), StateWorking},
			{42_000, ev(EvIdleMarker), StateIdle},
		}},
		{"permission prompt (claude bash) needs two sightings", working(), []tick{
			{0, promptEv(ps["claude_bash_test"]), StateWorking},
			{250, promptEv(ps["claude_bash_test"]), StateWaitingForPermission},
		}},
		{"workspace trust prompt is a permission", NewSnapshot(t0), []tick{
			{0, ev(EvProcessSpawn), StateWaiting},
			{100, promptEv(ps["agy_trust_own"]), StateWaiting},
			{400, promptEv(ps["agy_trust_own"]), StateWaitingForPermission},
		}},
		{"multi-choice menu is a decision", working(), []tick{
			{0, promptEv(ps["decision_plan_choice"]), StateWorking},
			{300, promptEv(ps["decision_plan_choice"]), StateWaitingForDecision},
		}},
		{"unconfident prompt → needs_attention (debounced)", working(), []tick{
			{0, promptEv(ps["drift_fastbrain_unverified"]), StateWorking},
			{1000, promptEv(ps["drift_fastbrain_unverified"]), StateWorking},
			{2100, promptEv(ps["drift_fastbrain_unverified"]), StateNeedsAttention},
		}},
		{"unknown-class prompt → needs_attention", working(), []tick{
			{0, promptEv(ps["drift_unknown_class"]), StateWorking},
			{2500, promptEv(ps["drift_unknown_class"]), StateNeedsAttention},
		}},
		{"answered prompt: cleared + output → working", Snapshot{State: StateWaitingForPermission, Since: t0}, []tick{
			{0, ev(EvPromptCleared), StateWaitingForPermission},
			{50, ev(EvPromptCleared, EvOutputActivity), StateWorking},
		}},
		{"rate limit banner → waiting{rate_limit}", working(), []tick{
			{0, []Evidence{{Kind: EvLimitBanner, Reason: ReasonRateLimit}}, StateWorking},
			{1200, []Evidence{{Kind: EvLimitBanner, Reason: ReasonRateLimit}}, StateWaiting},
		}},
		{"banner without a typed reason is ignored", working(), []tick{
			{0, ev(EvLimitBanner), StateWorking},
			{2000, ev(EvLimitBanner), StateWorking},
		}},
		{"hook is strong evidence", working(), []tick{
			{0, []Evidence{{Kind: EvHook, Hook: StateIdle}}, StateIdle},
			{10, []Evidence{{Kind: EvHook, Hook: StateWaiting, Reason: ReasonAuth}}, StateWaiting},
		}},
		{"crash: exit is immediate and sticky until respawn", working(), []tick{
			{0, ev(EvProcessExit), StateExited},
			{10, ev(EvWorkingMarker), StateExited},
			{20, promptEv(ps["claude_read"]), StateExited},
			{30, ev(EvProcessSpawn), StateWaiting},
			{40, ev(EvReady), StateIdle},
		}},
		{"recovered runtime starts unknown, needs evidence", NewSnapshot(t0), []tick{
			{0, silence(time.Minute), StateUnknown},
			{10, ev(EvWorkingMarker), StateWorking},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runTicks(t, c.start, c.ticks) })
	}
}

func TestStepReportsEvidence(t *testing.T) {
	s, ch := Step(working(), Observation{At: t0, Evidence: silence(6*time.Second, Evidence{Kind: EvIdleMarker})}, DefaultHysteresis)
	if ch == nil || ch.From != StateWorking || ch.To != StateIdle || s.Since != t0 {
		t.Fatalf("change = %+v snapshot=%+v", ch, s)
	}
	if len(ch.Evidence) != 2 || ch.Evidence[0] != EvIdleMarker || ch.Evidence[1] != EvSilence {
		t.Fatalf("evidence = %v", ch.Evidence)
	}
	// The committed change is a valid journal event payload.
	e := Event{V: SchemaVersion, SessionID: "s", Seq: 1, Kind: KindState, State: ch, Redacted: true}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
}
