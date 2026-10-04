package poller

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/srjn45/warden/internal/store"
)

// Anomaly is a non-fatal health warning the poller raises about an agent —
// distinct from a status transition (which classify already covers). The daemon
// wires OnAnomaly to surface it to the user (notification). The poller itself
// always records a durable "anomaly" event, so the signal survives even when no
// notifier is wired. Kind is one of the anomaly* constants.
type Anomaly struct {
	Kind   string // anomalyOOM | anomalyLoop | anomalyPreCrash | anomalyApprovalLoop
	Detail string // human-readable, ready to show in a notification or event log
}

const (
	// anomalyOOM marks a crash whose exit code matches the OOM-kill signature.
	anomalyOOM = "oom"
	// anomalyLoop marks an agent whose pane keeps churning the same output —
	// busy but making no progress, which the quiet-stuck timer never catches.
	anomalyLoop = "loop"
	// anomalyPreCrash marks a live agent at critical context that cannot be
	// auto-compacted (it is still working), warning the operator to /compact it
	// before the growing context window crashes the process.
	anomalyPreCrash      = "context_precrash"
	anomalyCompactFailed = "context_compact_failed"
	// anomalyApprovalLoop marks an agent whose identical approval prompt kept
	// re-appearing after being auto-approved: the circuit breaker halted further
	// approvals and the prompt now waits for a human.
	anomalyApprovalLoop = "approval_loop"
)

// oomExitCode is the shell exit status of a process killed by SIGKILL (128+9).
// The Linux OOM killer terminates a runaway process with SIGKILL, so a crash
// carrying this code is the cheapest strong signal that the agent was
// OOM-killed. It is a heuristic — a manual `kill -9` yields the same code — so
// the surfaced wording says "possible".
const oomExitCode = 137

// looksLikeOOM reports whether a crash exit code matches the OOM-kill signature.
func looksLikeOOM(code int) bool { return code == oomExitCode }

// crashAnomaly classifies a crash exit code into an optional health anomaly
// worth surfacing beyond the generic "session exited" event that FinalizeExit
// already records. ok=false means the crash carries no extra signal (the plain
// exit event already covers it), so the caller raises nothing.
func crashAnomaly(code int) (a Anomaly, ok bool) {
	if looksLikeOOM(code) {
		return Anomaly{
			Kind:   anomalyOOM,
			Detail: "possible OOM kill — agent terminated by SIGKILL (exit 137); reduce its context/memory or run fewer agents",
		}, true
	}
	return Anomaly{}, false
}

// crashTriageTimeout bounds one Fast-Brain crash diagnosis.
const crashTriageTimeout = 60 * time.Second

// signalFromExit names the fatal signal for a shell-style 128+N exit code.
func signalFromExit(code int) string {
	if code > 128 && code < 160 {
		return fmt.Sprintf("signal %d", code-128)
	}
	return ""
}

// triageCrash diagnoses a freshly crashed agent once (the caller only invokes
// it on the winning FinalizeExit CAS) via Fast-Brain. It runs off the tick
// goroutine and is fail-soft: any error is logged and swallowed. A warden bug
// is staged locally (never uploaded) and announced with a durable event so the
// operator can run `warden bug-report <id>`.
func (p *Poller) triageCrash(s *agentstore.Agent, code int) {
	if p.FastBrain == nil || s == nil {
		return
	}
	excerpt := s.LastPaneExcerpt
	if captured, err := p.deps.CapturePane(context.Background(), s.TmuxSession); err == nil && strings.TrimSpace(captured) != "" {
		excerpt = captured
	}
	excerpt = lastLines(excerpt, fastbrain.ExcerptLines)
	dir := p.CrashDir
	if dir == "" {
		d, err := fastbrain.DefaultCrashDir()
		if err != nil {
			slog.Warn("poller: crash triage skipped", "agent", s.ID, "err", err)
			return
		}
		dir = d
	}
	in := fastbrain.CrashInput{
		AgentID: s.ID, ExitCode: code, Signal: signalFromExit(code), Command: s.AiCli,
		Excerpt: excerpt, Version: p.Version, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, StageDir: dir,
	}
	p.triageWG.Add(1)
	go func() {
		defer p.triageWG.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Warn("poller: crash triage panicked", "agent", s.ID, "panic", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), crashTriageTimeout)
		defer cancel()
		dg, err := fastbrain.DiagnoseFailure(ctx, p.FastBrain, in)
		if err != nil {
			slog.Warn("poller: crash triage failed", "agent", s.ID, "err", err)
			return
		}
		switch {
		case dg.IsWardenBug && dg.DraftPath != "":
			detail := fmt.Sprintf("warden bug draft staged (%s); review with: warden bug-report %s", dg.Draft.ID, dg.Draft.ID)
			_ = p.deps.RecordEvent(ctx, s.ID, store.Event{Type: "bug_draft_staged", Detail: detail})
		case dg.SuggestSwitchBackend:
			_ = p.deps.RecordEvent(ctx, s.ID, store.Event{Type: "crash_triage", Detail: "transient error — consider switching backend"})
		}
	}()
}
