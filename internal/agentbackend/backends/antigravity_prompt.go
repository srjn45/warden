package backends

import (
	"strings"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/ptyevent"
)

// ShapePrompt implements agentbackend.PromptShaper. It states what `agy`'s
// wording means in the normalized prompt contract so the router never has to
// guess from generic heuristics:
//
//   - the tool-permission menu: "Yes…" is a grant (once / this conversation /
//     persisted to settings.json), "No…" is a refusal — including "No, cancel",
//     which declines the one request and is NOT the contract's cancel (that is
//     the footer's "esc to cancel", which aborts the turn);
//   - the workspace-trust prompt: "Yes, I trust this folder" is a standing
//     workspace grant, "No, exit" quits the CLI (cancel, never an auto-deny).
//
// Any option it cannot classify makes the whole shape unavailable (ok=false)
// rather than leaving a guessed meaning on the table.
func (Antigravity) ShapePrompt(app *agentbackend.Approval) (agentbackend.PromptShape, bool) {
	if app == nil || len(app.Options) < 2 {
		return agentbackend.PromptShape{}, false
	}
	sh := agentbackend.PromptShape{
		Class:   ptyevent.ClassPermission,
		Options: make([]agentbackend.OptionShape, len(app.Options)),
		Cancel:  ptyevent.CancelPath{Key: "Escape"},
	}

	trust := app.Kind == agentbackend.ApprovalKindTrust
	switch {
	case trust:
		sh.Subject, sh.Target = "workspace", app.Action
		if sh.Target == "" {
			return agentbackend.PromptShape{}, false // no directory under question
		}
		sh.Caps = []ptyevent.Capability{ptyevent.CapTrust}
	case strings.HasPrefix(app.Action, "Read:"):
		sh.Subject, sh.Target = "Read", strings.TrimSpace(strings.TrimPrefix(app.Action, "Read:"))
		sh.Caps = []ptyevent.Capability{ptyevent.CapRead}
	case strings.HasPrefix(app.Action, "Write:"):
		sh.Subject, sh.Target = "Write", strings.TrimSpace(strings.TrimPrefix(app.Action, "Write:"))
		sh.Caps = []ptyevent.Capability{ptyevent.CapWrite}
	case app.Action != "":
		// "Requesting permission for:" echoes a shell command.
		sh.Subject, sh.Target = "Bash", app.Action
		sh.Caps = []ptyevent.Capability{ptyevent.CapExecute}
	default:
		return agentbackend.PromptShape{}, false // nothing identifies what is asked
	}
	if sh.Target == "" {
		return agentbackend.PromptShape{}, false
	}

	for i, label := range app.Options {
		os, ok := agyOptionShape(label, trust)
		if !ok {
			return agentbackend.PromptShape{}, false
		}
		sh.Options[i] = os
		if trust && os.Semantic == ptyevent.SemCancel {
			sh.Cancel = ptyevent.CancelPath{Key: "Escape", OptionIndex: i + 1}
		}
	}
	return sh, true
}

// agyOptionShape classifies one `agy` option label. ok=false for any wording
// outside the captured vocabulary (see testdata/antigravity).
func agyOptionShape(label string, trust bool) (agentbackend.OptionShape, bool) {
	low := strings.ToLower(strings.TrimSpace(label))
	if trust {
		switch {
		case strings.HasPrefix(low, "yes") && strings.Contains(low, "trust"):
			return agentbackend.OptionShape{Semantic: ptyevent.SemAllowAlways, Scope: ptyevent.ScopeWorkspace}, true
		case strings.HasPrefix(low, "no") && strings.Contains(low, "exit"):
			return agentbackend.OptionShape{Semantic: ptyevent.SemCancel}, true
		}
		return agentbackend.OptionShape{}, false
	}
	switch {
	case strings.HasPrefix(low, "yes"):
		switch {
		case strings.Contains(low, "persist to settings"):
			return agentbackend.OptionShape{Semantic: ptyevent.SemAllowAlways, Scope: ptyevent.ScopeGlobal}, true
		case strings.Contains(low, "always allow") || strings.Contains(low, "don't ask again"):
			return agentbackend.OptionShape{Semantic: ptyevent.SemAllowSession, Scope: ptyevent.ScopeSession}, true
		case low == "yes", low == "yes, run command", low == "yes, allow access", low == "yes, proceed":
			return agentbackend.OptionShape{Semantic: ptyevent.SemAllowOnce, Scope: ptyevent.ScopeOnce}, true
		}
	case strings.HasPrefix(low, "no"):
		switch low {
		case "no", "no, cancel", "no, deny access", "no, deny":
			return agentbackend.OptionShape{Semantic: ptyevent.SemDeny}, true
		}
	}
	return agentbackend.OptionShape{}, false
}

// PostureSupport implements agentbackend.PostureMapper. `agy`'s own modes map
// exactly for three postures; allowlisted degrades because the adapter's
// ordinary (default) launch is already a native bypass (see agyPermFlag) — an
// allowlist cannot be enforced natively there, so it is enforced from the
// prompt stream for whatever prompts still appear.
func (a Antigravity) PostureSupport(p ptyevent.Posture) agentbackend.PostureSupport {
	switch p {
	case ptyevent.PostureReadOnly:
		return agentbackend.PostureSupport{Posture: p, Mode: "plan", Native: true}
	case ptyevent.PostureReadWrite:
		return agentbackend.PostureSupport{Posture: p, Mode: "accept-edits", Native: true}
	case ptyevent.PostureDangerouslyExecuteAll:
		return agentbackend.PostureSupport{Posture: p, Mode: "dangerously-skip-permissions", Native: true}
	case ptyevent.PostureAllowlisted:
		return agentbackend.PostureSupport{
			Posture: p, Mode: "default", Degraded: true,
			Note: "agy's default launch is native bypass; the allowlist is enforced only on prompts that still reach the PTY",
		}
	}
	return agentbackend.PostureSupport{Posture: p, Degraded: true, Note: "unknown posture"}
}
