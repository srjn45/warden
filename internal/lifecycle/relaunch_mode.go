package lifecycle

import (
	"errors"
	"fmt"
	"strings"

	"github.com/srjn45/warden/internal/agentbackend"
)

// ErrNoSafeMode is returned by ResolveRelaunchMode when the target backend has no
// permission mode that grants no more than the stored intent. The caller must
// refuse the relaunch rather than guess.
var ErrNoSafeMode = errors.New("no permission mode on target backend is at most as permissive as the stored intent")

// RelaunchOutcome says what the resolver did to the stored mode.
type RelaunchOutcome string

const (
	// OutcomeKept: the stored mode is valid for the target and was returned verbatim.
	OutcomeKept RelaunchOutcome = "kept"
	// OutcomeTranslated: re-expressed in the target's vocabulary at the same intent.
	OutcomeTranslated RelaunchOutcome = "translated"
	// OutcomeSteppedDown: the target has no equivalent; a strictly weaker intent was used.
	OutcomeSteppedDown RelaunchOutcome = "stepped-down"
	// OutcomeDefaulted: no stored mode; the role/config default was resolved for the target.
	OutcomeDefaulted RelaunchOutcome = "defaulted"
)

// Intent classification sources, recorded in RelaunchRationale.IntentSource.
const (
	IntentSourceBackend = "backend-table"   // the stored backend's own ModeTable
	IntentSourceLegacy  = "legacy-table"    // legacyModeIntents
	IntentSourceForeign = "foreign-table"   // every registered backend that knows it agrees
	IntentSourceRole    = "role-default"    // stored empty; the role's default for the backend
	IntentSourceConfig  = "config-default"  // stored empty; configured default mode
	IntentSourceBuiltin = "builtin-default" // stored empty and no role/config default
	IntentSourceUnknown = "unknown"         // unclassifiable; conservatively IntentDefault
)

// RelaunchModeInput is everything the resolver needs; it reads no state.
type RelaunchModeInput struct {
	// StoredMode is the agent's persisted permission mode (may be empty).
	StoredMode string
	// StoredBackend is the backend whose vocabulary StoredMode is expected to be in
	// (the agent's own backend). Nil resolves to the default backend.
	StoredBackend agentbackend.Backend
	// Target is the backend the relaunch runs on (== StoredBackend except hot-swap).
	Target agentbackend.Backend
	// Role is the agent's role ("" = general). It may only tighten the result.
	Role string
	// DefaultMode is the configured default permission mode (Claude vocabulary),
	// used when StoredMode is empty and the role has no default.
	DefaultMode string
}

// RelaunchRationale is the structured explanation of a resolution.
type RelaunchRationale struct {
	StoredMode    string
	StoredBackend string
	TargetBackend string
	Role          string
	// StoredIntent is the intent the stored mode (or its default) classified to.
	StoredIntent agentbackend.PermissionIntent
	IntentSource string
	// AcceptedIntent is the intent of the returned mode (<= StoredIntent).
	AcceptedIntent agentbackend.PermissionIntent
	Outcome        RelaunchOutcome
	// RoleTightened is true when the role posture lowered the intent.
	RoleTightened bool
	// Reasons are human-readable steps, in order.
	Reasons []string
}

// Changed reports whether the returned mode differs from the stored one.
func (r RelaunchRationale) Changed(mode string) bool { return mode != r.StoredMode }

// Note renders the rationale as one line; "" when nothing was changed.
func (r RelaunchRationale) Note() string {
	if r.Outcome == OutcomeKept {
		return ""
	}
	return strings.Join(r.Reasons, "; ")
}

// legacyModeIntents classifies persisted values the backends' own tables omit
// (docs/specs/2026-10-10-relaunch-permission-mode-inventory.md §3.4). Keyed by
// backend id, because "auto" means different things on claude and goose. Values
// are conservative: nothing here classifies as more permissive than its meaning.
var legacyModeIntents = map[string]map[string]agentbackend.PermissionIntent{
	"claude":      {"auto": agentbackend.IntentAcceptEdits, "dontAsk": agentbackend.IntentReadOnly},
	"cursor":      {"auto-review": agentbackend.IntentDefault},
	"goose":       {"smart_approve": agentbackend.IntentDefault},
	"antigravity": {"sandbox": agentbackend.IntentDefault, "proceed-in-sandbox": agentbackend.IntentDefault, "acceptEdits": agentbackend.IntentAcceptEdits},
	"codex":       {"workspace-write": agentbackend.IntentAcceptEdits},
}

// intentDescent lists, per intent, the intents to try in order: the intent itself,
// then strictly weaker ones. Never contains a stronger intent.
var intentDescent = map[agentbackend.PermissionIntent][]agentbackend.PermissionIntent{
	agentbackend.IntentSkipAll:     {agentbackend.IntentSkipAll, agentbackend.IntentAcceptEdits, agentbackend.IntentDefault, agentbackend.IntentPlan, agentbackend.IntentReadOnly},
	agentbackend.IntentAcceptEdits: {agentbackend.IntentAcceptEdits, agentbackend.IntentDefault, agentbackend.IntentPlan, agentbackend.IntentReadOnly},
	agentbackend.IntentDefault:     {agentbackend.IntentDefault, agentbackend.IntentPlan, agentbackend.IntentReadOnly},
	agentbackend.IntentPlan:        {agentbackend.IntentPlan, agentbackend.IntentReadOnly},
	agentbackend.IntentReadOnly:    {agentbackend.IntentReadOnly, agentbackend.IntentPlan},
}

// classifyOwn classifies mode in b's vocabulary via its ModeTable then the legacy table.
func classifyOwn(b agentbackend.Backend, mode string) (agentbackend.PermissionIntent, string, bool) {
	if b == nil || mode == "" {
		return "", "", false
	}
	if pm, ok := b.(agentbackend.PermissionMapper); ok {
		if i, ok := pm.ModeIntent(mode); ok {
			return i, IntentSourceBackend, true
		}
	}
	if i, ok := legacyModeIntents[b.ID()][mode]; ok {
		return i, IntentSourceLegacy, true
	}
	return "", "", false
}

// classifyForeign classifies mode by asking every registered backend; it succeeds
// only if every backend that knows the mode agrees (and at least one does).
func classifyForeign(mode string) (agentbackend.PermissionIntent, bool) {
	var found agentbackend.PermissionIntent
	for _, id := range agentbackend.IDs() {
		b, err := agentbackend.Get(id)
		if err != nil {
			continue
		}
		i, _, ok := classifyOwn(b, mode)
		if !ok {
			continue
		}
		if found != "" && found != i {
			return "", false
		}
		found = i
	}
	return found, found != ""
}

// roleIntentOn returns the intent of the role's default mode on b, if the role has one.
func roleIntentOn(role string, b agentbackend.Backend) (agentbackend.PermissionIntent, bool) {
	if role == "" || b == nil {
		return "", false
	}
	req := &SpawnRequest{Role: role, Backend: b.ID()}
	applyRoleBackendMode(req)
	i, _, ok := classifyOwn(b, req.PermissionMode)
	return i, ok
}

// ResolveRelaunchMode picks the permission mode an existing agent relaunches
// with on the target backend. The returned mode is always in the target's
// vocabulary and never more permissive than the stored intent; ErrNoSafeMode is
// returned if none exists. It is pure: it reads no state and persists nothing —
// callers persist a corrected mode only after the launch succeeds.
func ResolveRelaunchMode(in RelaunchModeInput) (string, RelaunchRationale, error) {
	stored := in.StoredBackend
	if stored == nil {
		stored = in.Target
	}
	rat := RelaunchRationale{StoredMode: in.StoredMode, Role: in.Role}
	if stored != nil {
		rat.StoredBackend = stored.ID()
	}
	if in.Target == nil {
		return "", rat, fmt.Errorf("resolve relaunch mode: no target backend")
	}
	target := in.Target
	rat.TargetBackend = target.ID()
	sameBackend := stored != nil && stored.ID() == target.ID()

	// 1. Classify the stored intent.
	var intent agentbackend.PermissionIntent
	storedValid := false // stored mode is a classified member of the stored backend's vocabulary
	switch {
	case in.StoredMode != "":
		if i, src, ok := classifyOwn(stored, in.StoredMode); ok {
			intent, rat.IntentSource = i, src
			storedValid = agentbackend.ModeAccepted(stored, in.StoredMode)
		} else if i, ok := classifyForeign(in.StoredMode); ok {
			intent, rat.IntentSource = i, IntentSourceForeign
			rat.Reasons = append(rat.Reasons, fmt.Sprintf("permission_mode %q is not a %s mode; classified as %s from other backends", in.StoredMode, rat.StoredBackend, i))
		} else {
			intent, rat.IntentSource = agentbackend.IntentDefault, IntentSourceUnknown
			rat.Reasons = append(rat.Reasons, fmt.Sprintf("permission_mode %q is unrecognised; treated as %s", in.StoredMode, intent))
		}
	default:
		if i, ok := roleIntentOn(in.Role, stored); ok {
			intent, rat.IntentSource = i, IntentSourceRole
		} else if cb, err := agentbackend.Get(agentbackend.DefaultID); err == nil && in.DefaultMode != "" {
			if i, _, ok := classifyOwn(cb, in.DefaultMode); ok {
				intent, rat.IntentSource = i, IntentSourceConfig
			}
		}
		if intent == "" {
			intent, rat.IntentSource = agentbackend.IntentDefault, IntentSourceBuiltin
		}
		rat.Reasons = append(rat.Reasons, fmt.Sprintf("no stored permission_mode; resolved %s from %s", intent, rat.IntentSource))
	}
	rat.StoredIntent = intent

	// 2. Role posture may only tighten.
	if ri, ok := roleIntentOn(in.Role, target); ok && ri.Rank() < intent.Rank() {
		rat.RoleTightened = true
		rat.Reasons = append(rat.Reasons, fmt.Sprintf("role %q posture tightened intent %s to %s", in.Role, intent, ri))
		intent = ri
	}

	// 3. Same backend, valid stored mode, no tightening: keep byte-for-byte.
	if sameBackend && storedValid && !rat.RoleTightened {
		rat.AcceptedIntent, rat.Outcome = intent, OutcomeKept
		return in.StoredMode, rat, nil
	}

	// 4. Render for the target, stepping only downward.
	tm, ok := target.(agentbackend.PermissionMapper)
	if !ok {
		return "", rat, fmt.Errorf("%w: backend %s has no mode mapping", ErrNoSafeMode, target.ID())
	}
	for n, cand := range intentDescent[intentKey(intent)] {
		m, ok := tm.ModeForIntent(cand)
		if !ok || m == "" || !agentbackend.ModeAccepted(target, m) {
			continue
		}
		back, ok := tm.ModeIntent(m)
		if !ok {
			back, _, ok = classifyOwn(target, m)
		}
		if !ok || !back.AtMost(intent) {
			continue
		}
		rat.AcceptedIntent = back
		switch {
		case in.StoredMode == "":
			rat.Outcome = OutcomeDefaulted
		case n > 0 && back != intent:
			rat.Outcome = OutcomeSteppedDown
			rat.Reasons = append(rat.Reasons, fmt.Sprintf("%s has no %s mode; stepped down to %s (%q)", target.ID(), intent, back, m))
		default:
			rat.Outcome = OutcomeTranslated
		}
		if in.StoredMode != "" && m == in.StoredMode && rat.Outcome == OutcomeTranslated {
			rat.Outcome = OutcomeKept
			return m, rat, nil
		}
		if in.StoredMode != "" && rat.Outcome == OutcomeTranslated {
			rat.Reasons = append(rat.Reasons, fmt.Sprintf("permission_mode %q (%s) translated to %q (%s) by intent", in.StoredMode, rat.StoredBackend, m, target.ID()))
		}
		return m, rat, nil
	}
	return "", rat, fmt.Errorf("%w: backend %s, intent %s", ErrNoSafeMode, target.ID(), intent)
}

// intentKey normalises an unknown intent to the strictest descent (fail closed
// on lookup: an unclassifiable intent is treated as skip-all rank, so it gets
// the full descent and the AtMost check bounds it).
func intentKey(i agentbackend.PermissionIntent) agentbackend.PermissionIntent {
	if _, ok := intentDescent[i]; ok {
		return i
	}
	return agentbackend.IntentSkipAll
}
