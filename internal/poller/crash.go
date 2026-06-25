package poller

// Anomaly is a non-fatal health warning the poller raises about an agent —
// distinct from a status transition (which classify already covers). The daemon
// wires OnAnomaly to surface it to the user (notification). The poller itself
// always records a durable "anomaly" event, so the signal survives even when no
// notifier is wired. Kind is one of the anomaly* constants.
type Anomaly struct {
	Kind   string // anomalyOOM | anomalyLoop | anomalyPreCrash
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
	anomalyPreCrash = "context_precrash"
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
