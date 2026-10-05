package agentbackend

// PermissionIntent is the backend-neutral meaning of a backend-native permission
// mode. A cross-backend hot-swap translates a stored mode by intent (claude's
// "bypassPermissions" and antigravity's "dangerously-skip-permissions" are both
// IntentSkipAll) so the successor never receives a mode string from another
// backend's vocabulary. Each backend owns its own table (ModeTable); the swap
// code carries no per-backend string special-cases.
type PermissionIntent string

const (
	IntentDefault     PermissionIntent = "default"      // the backend's ordinary approval posture
	IntentPlan        PermissionIntent = "plan"         // plan / chat-only, no edits
	IntentReadOnly    PermissionIntent = "read-only"    // may read but not write
	IntentAcceptEdits PermissionIntent = "accept-edits" // edits auto-approved, other actions still gated
	IntentSkipAll     PermissionIntent = "skip-all"     // every permission prompt skipped
)

// PermissionMapper is an optional Backend extension: a backend that implements it
// can classify its own modes and render an intent as one of its own modes.
type PermissionMapper interface {
	// ModeIntent classifies one of this backend's modes. ok=false for a mode with
	// no neutral equivalent (so a swap must not guess).
	ModeIntent(mode string) (PermissionIntent, bool)
	// ModeForIntent returns this backend's mode for the intent. ok=false when the
	// backend has no equivalent.
	ModeForIntent(intent PermissionIntent) (string, bool)
}

// ModeTable is the shared table implementation backends delegate to.
type ModeTable struct {
	ToIntent   map[string]PermissionIntent // native mode → intent
	FromIntent map[PermissionIntent]string // intent → native mode
}

// Intent classifies a native mode.
func (t ModeTable) Intent(mode string) (PermissionIntent, bool) {
	i, ok := t.ToIntent[mode]
	return i, ok
}

// ForIntent renders an intent as a native mode.
func (t ModeTable) ForIntent(i PermissionIntent) (string, bool) {
	m, ok := t.FromIntent[i]
	return m, ok
}

// TranslateMode maps mode from one backend's vocabulary into to's by intent. It
// returns ok=false when from cannot classify the mode or to has no equivalent;
// the caller then falls back to a default rather than guessing. A backend that
// does not implement PermissionMapper never yields a translation.
func TranslateMode(from, to Backend, mode string) (string, bool) {
	fm, ok := from.(PermissionMapper)
	if !ok {
		return "", false
	}
	tm, ok := to.(PermissionMapper)
	if !ok {
		return "", false
	}
	intent, ok := fm.ModeIntent(mode)
	if !ok {
		return "", false
	}
	return tm.ModeForIntent(intent)
}

// ModeAccepted reports whether mode is a member of b's declared vocabulary.
func ModeAccepted(b Backend, mode string) bool {
	for _, m := range b.Capabilities().PermissionModes {
		if m == mode {
			return true
		}
	}
	return false
}
