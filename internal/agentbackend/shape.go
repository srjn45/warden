package agentbackend

import (
	"fmt"

	"github.com/srjn45/warden/internal/ptyevent"
)

// PromptShape is an adapter's reading of one parsed Approval in the vocabulary
// of the normalized prompt contract (docs/specs/2026-10-19-pty-runtime-contract.md
// §9). The backend parser knows what its own wording means; the shared router
// only knows generic heuristics. A shape lets the adapter state the facts it is
// certain of and leaves everything else to the router's conservative inference.
//
// An adapter MUST describe every option (len(Options) == len(Approval.Options))
// or return ok=false: a partial or misaligned shape is discarded by
// ValidateShape so a drifted parser can never leave an option with an invented
// meaning.
type PromptShape struct {
	// Class is the permission-vs-decision verdict.
	Class ptyevent.PromptClass
	// Subject is who asks ("Bash", "Read", "workspace"); Target what it acts on.
	Subject string
	Target  string
	// Caps are capabilities the adapter is certain of. The router unions them
	// with target-derived heuristics (it may add, e.g. publish for `git push`,
	// but never removes one the adapter named).
	Caps []ptyevent.Capability
	// Options describes each option top-down, aligned with Approval.Options.
	Options []OptionShape
	// Cancel is how to back out without choosing. Zero means Escape.
	Cancel ptyevent.CancelPath
}

// OptionShape is the contract meaning of one rendered option.
type OptionShape struct {
	Semantic ptyevent.OptionSemantic
	Scope    ptyevent.Scope
	// Key is the hotkey when the menu is not cursor-navigated.
	Key string
}

// PromptShaper is an optional Backend extension: the adapter maps its own
// parsed Approval into the normalized prompt vocabulary.
type PromptShaper interface {
	// ShapePrompt returns the adapter's reading of app. ok=false means the
	// adapter has no confident reading; the router then falls back to generic
	// inference, which for an ambiguous menu fails to needs_attention.
	ShapePrompt(app *Approval) (PromptShape, bool)
}

// ValidateShape checks that a shape is internally consistent and aligned with
// the approval it describes.
func ValidateShape(sh PromptShape, app *Approval) error {
	if app == nil {
		return fmt.Errorf("shape: nil approval")
	}
	if !sh.Class.Valid() || sh.Class == ptyevent.ClassUnknown {
		return fmt.Errorf("shape: class %q", sh.Class)
	}
	if len(sh.Options) != len(app.Options) {
		return fmt.Errorf("shape: describes %d options, approval has %d", len(sh.Options), len(app.Options))
	}
	for i, o := range sh.Options {
		if !o.Semantic.Valid() || !o.Scope.Valid() {
			return fmt.Errorf("shape: option %d semantic=%q scope=%q", i+1, o.Semantic, o.Scope)
		}
	}
	for _, c := range sh.Caps {
		if !c.Valid() {
			return fmt.Errorf("shape: capability %q", c)
		}
	}
	if sh.Class == ptyevent.ClassPermission && len(sh.Caps) == 0 {
		return fmt.Errorf("shape: permission names no capability")
	}
	if ci := sh.Cancel.OptionIndex; ci < 0 || ci > len(app.Options) {
		return fmt.Errorf("shape: cancel option %d", ci)
	}
	return nil
}

// PostureSupport is how a backend realizes a provider-neutral permission
// posture natively. It makes the audit explicit: every (backend, posture) pair
// either maps to an exact native mode or is declared Degraded with the reason,
// never silently approximated.
type PostureSupport struct {
	Posture ptyevent.Posture
	// Mode is the native launch mode that best realizes the posture ("" = the
	// backend's ordinary default launch).
	Mode string
	// Native is true when the backend enforces the posture itself: its own mode
	// already grants (or withholds) exactly that authority, so warden only has
	// to answer the prompts the mode leaves behind (trust, strategic menus).
	Native bool
	// Degraded is true when no native mode matches. Warden then relies on the
	// PTY router alone to enforce the posture from the prompt stream, which is
	// always safe (the router decides every prompt) but is not defense in depth.
	Degraded bool
	// Note explains a degradation in operator-readable words.
	Note string
}

// PostureMapper is an optional Backend extension for backends whose posture
// mapping is not derivable from their ModeTable.
type PostureMapper interface {
	PostureSupport(p ptyevent.Posture) PostureSupport
}

// SupportForPosture reports how b realizes p. It prefers an explicit
// PostureMapper, then derives from the backend's ModeTable by intent. A posture
// with no native equivalent degrades to the backend's default mode with the
// router enforcing the posture.
func SupportForPosture(b Backend, p ptyevent.Posture) PostureSupport {
	if pm, ok := b.(PostureMapper); ok {
		return pm.PostureSupport(p)
	}
	intent, ok := intentForPosture(p)
	if !ok {
		return PostureSupport{Posture: p, Degraded: true, Note: "unknown posture; router decides every prompt"}
	}
	mapper, isMapper := b.(PermissionMapper)
	if !isMapper {
		return PostureSupport{Posture: p, Degraded: true, Note: "backend declares no permission modes; router decides every prompt"}
	}
	if mode, ok := mapper.ModeForIntent(intent); ok {
		// allowlisted is warden's own rule engine over the backend's ordinary
		// prompting, so its default mode is not a native enforcement of it.
		return PostureSupport{Posture: p, Mode: mode, Native: p != ptyevent.PostureAllowlisted}
	}
	return PostureSupport{
		Posture:  p,
		Degraded: true,
		Note:     fmt.Sprintf("no native %s mode; launching default and enforcing %s from the prompt stream", intent, p),
	}
}

// intentForPosture is the inverse of PostureForIntent. allowlisted has no
// native counterpart by design: it is warden's own rule engine over the
// backend's ordinary prompting, so it launches the backend's default mode.
func intentForPosture(p ptyevent.Posture) (PermissionIntent, bool) {
	switch p {
	case ptyevent.PostureReadOnly:
		return IntentReadOnly, true
	case ptyevent.PostureReadWrite:
		return IntentAcceptEdits, true
	case ptyevent.PostureAllowlisted:
		return IntentDefault, true
	case ptyevent.PostureDangerouslyExecuteAll:
		return IntentSkipAll, true
	}
	return "", false
}
