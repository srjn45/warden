package ptyhost

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/store"
)

// ErrAdapterNotDirectCapable is returned when a direct PTY launch is requested
// for an adapter that is not direct-capable per §6.5.
var ErrAdapterNotDirectCapable = errors.New("ptyhost: adapter not direct-capable")

// IsAdapterDirectCapable reports whether an adapter is capable of direct PTY launch.
// Per §6.5 matrix at baseline, only shell ("terminal") is direct-capable;
// claude, antigravity, codex, cursor, aider, opencode, crush, goose are not yet direct-capable.
func IsAdapterDirectCapable(adapter string) bool {
	switch strings.ToLower(adapter) {
	case "terminal", "shell", "":
		return true
	default:
		return false
	}
}

// ValidateAdapterDirect checks whether an adapter is capable of direct launch,
// returning ErrAdapterNotDirectCapable if it is not.
func ValidateAdapterDirect(adapter string) error {
	if !IsAdapterDirectCapable(adapter) {
		return fmt.Errorf("%w: %s", ErrAdapterNotDirectCapable, adapter)
	}
	return nil
}

// RuntimeInfo is the operator- and API-facing runtime identity and capability
// model resolved by Manager.Resolve (§6.2).
type RuntimeInfo struct {
	Kind          string      `json:"kind"`                  // "direct" | "tmux" | "unknown" (actual, not requested)
	Presentation  string      `json:"presentation"`          // "direct" | "tmux" (= ptyevent.Presentation)
	Key           string      `json:"key"`                   // runtime key
	ProcessOwner  string      `json:"process_owner"`         // "holder" | "daemon" | "tmux" | "none"
	Holder        *HolderInfo `json:"holder,omitempty"`      // only for holder-hosted
	PinnedMode    string      `json:"pinned_mode,omitempty"` // per-session pin (durable)
	RequestedMode string      `json:"requested_mode"`        // current global (informational)
	Authoritative bool        `json:"authoritative"`         // today: IsAuthoritative
	Capabilities  []string    `json:"capabilities"`          // §6.5; honest, per-session
	Source        string      `json:"source"`                // "live" | "record" | "legacy-probe"
	Reason        string      `json:"reason,omitempty"`      // actionable reason when unknown or degraded
}

// HolderInfo is process identity for a holder-hosted session.
type HolderInfo struct {
	PID       int    `json:"pid"`
	StartTime uint64 `json:"start_time"`
	Proto     int    `json:"proto"`
}

// SessionRecord carries the durable record fields needed by Resolve.
type SessionRecord struct {
	ID          string
	Kind        string // "agent" or "terminal"
	AiCli       string // adapter name
	TmuxSession string // legacy runtime key
	Runtime     *store.SessionRuntime
}

// RuntimeKey returns the name under which the runtime indexes this session.
func (r *SessionRecord) RuntimeKey() string {
	if r == nil {
		return ""
	}
	if r.TmuxSession != "" {
		return r.TmuxSession
	}
	return r.ID
}

// RecordResolver queries durable store records for a session ID.
type RecordResolver func(ctx context.Context, id string) (*SessionRecord, bool)

// SetRecordResolver tells the manager how to look up persisted session records.
func (m *Manager) SetRecordResolver(fn RecordResolver) {
	m.rmu.Lock()
	m.recordResolver = fn
	m.rmu.Unlock()
}

func (m *Manager) getRecord(ctx context.Context, id string) (*SessionRecord, bool) {
	m.rmu.RLock()
	fn := m.recordResolver
	m.rmu.RUnlock()
	if fn == nil {
		return nil, false
	}
	return fn(ctx, id)
}

func computeCapabilities(kind string, survivesDaemon, authoritative bool) []string {
	switch kind {
	case "direct":
		caps := []string{"native-attach", "input", "resize"}
		if authoritative {
			caps = append(caps, "authoritative-state")
		}
		if survivesDaemon {
			caps = append(caps, "survives-daemon")
		}
		return caps
	case "tmux":
		return []string{"observe", "tmux-attach"}
	default:
		return []string{}
	}
}

// Resolve resolves a sessionID to its runtime identity and capabilities implementing §6.2 precedence:
// (1) live direct session in the manager → direct (Manager.Direct);
// (2) holder record exists and verifies (Launcher.Has + identity) → direct/holder;
// (3) live mirror (Manager.shadow.Mirror) and tmux has-session → tmux;
// (4) persisted record field when none of (1)–(3) is live (reports last-known kind with source:"record", authoritative:false);
// (5) tmux has-session on TmuxSession for records predating the field → tmux, source:"legacy-probe";
// else unknown with an actionable reason. It never infers from TmuxSession != "".
func (m *Manager) Resolve(ctx context.Context, sessionID string) RuntimeInfo {
	if ctx == nil {
		ctx = context.Background()
	}
	rec, hasRec := m.getRecord(ctx, sessionID)

	key := sessionID
	legacyKey := ""
	if hasRec {
		legacyKey = rec.RuntimeKey()
	}

	reqMode := ""
	if m != nil {
		reqMode = string(m.Mode())
	}

	// (1) Live direct session in the manager
	if m != nil && m.direct != nil {
		for _, k := range []string{key, legacyKey} {
			if k == "" {
				continue
			}
			if s := m.direct.Session(k); s != nil && s.Alive() {
				owner := "daemon"
				isHolder := m.direct.HolderHosted(k)
				if isHolder {
					owner = "holder"
				}
				var holder *HolderInfo
				if hv, ok := m.direct.Launcher.(interface {
					VerifyHolder(string) (int, uint64, int, bool)
				}); ok && isHolder {
					if pid, start, proto, ok := hv.VerifyHolder(k); ok {
						holder = &HolderInfo{PID: pid, StartTime: start, Proto: proto}
					}
				}
				pinned := ""
				if pin, ok := m.PinnedRuntime(k); ok {
					pinned = string(pin)
				} else if hasRec && rec.Runtime != nil && rec.Runtime.PinnedMode != "" {
					pinned = rec.Runtime.PinnedMode
				}
				return RuntimeInfo{
					Kind:          "direct",
					Presentation:  string(ptyevent.PresentationDirect),
					Key:           k,
					ProcessOwner:  owner,
					Holder:        holder,
					PinnedMode:    pinned,
					RequestedMode: reqMode,
					Authoritative: true,
					Capabilities:  computeCapabilities("direct", isHolder, true),
					Source:        "live",
				}
			}
		}
	}

	// (2) Holder record exists and verifies (Launcher.Has + identity) -> direct/holder
	if m != nil && m.direct != nil && m.direct.Launcher != nil {
		for _, k := range []string{key, legacyKey} {
			if k == "" {
				continue
			}
			if hv, ok := m.direct.Launcher.(interface {
				VerifyHolder(string) (int, uint64, int, bool)
			}); ok {
				if pid, start, proto, ok := hv.VerifyHolder(k); ok {
					pinned := ""
					if pin, ok := m.PinnedRuntime(k); ok {
						pinned = string(pin)
					} else if hasRec && rec.Runtime != nil && rec.Runtime.PinnedMode != "" {
						pinned = rec.Runtime.PinnedMode
					}
					return RuntimeInfo{
						Kind:          "direct",
						Presentation:  string(ptyevent.PresentationDirect),
						Key:           k,
						ProcessOwner:  "holder",
						Holder:        &HolderInfo{PID: pid, StartTime: start, Proto: proto},
						PinnedMode:    pinned,
						RequestedMode: reqMode,
						Authoritative: false,
						Capabilities:  computeCapabilities("direct", true, false),
						Source:        "live",
					}
				}
			}
		}
	}

	// (3) Live mirror (Manager.shadow.Mirror) and tmux has-session -> tmux
	if m != nil && m.shadow != nil && m.tmux != nil {
		for _, k := range []string{key, legacyKey} {
			if k == "" {
				continue
			}
			if mr := m.shadow.Mirror(k); mr != nil {
				if m.tmux.HasSession(ctx, k) {
					pinned := ""
					if pin, ok := m.PinnedRuntime(k); ok {
						pinned = string(pin)
					} else if hasRec && rec.Runtime != nil && rec.Runtime.PinnedMode != "" {
						pinned = rec.Runtime.PinnedMode
					}
					return RuntimeInfo{
						Kind:          "tmux",
						Presentation:  string(ptyevent.PresentationTmux),
						Key:           k,
						ProcessOwner:  "tmux",
						PinnedMode:    pinned,
						RequestedMode: reqMode,
						Authoritative: false,
						Capabilities:  computeCapabilities("tmux", false, false),
						Source:        "live",
					}
				}
			}
		}
	}

	// (4) Persisted record field when none of (1)-(3) is live
	if hasRec && rec.Runtime != nil && rec.Runtime.Kind != "" {
		kind := rec.Runtime.Kind
		pres := string(ptyevent.PresentationTmux)
		if kind == "direct" {
			pres = string(ptyevent.PresentationDirect)
		}
		owner := "none"
		if rec.Runtime.HolderPID > 0 {
			owner = "holder"
		} else if kind == "direct" {
			owner = "daemon"
		} else if kind == "tmux" {
			owner = "tmux"
		}
		var holder *HolderInfo
		if rec.Runtime.HolderPID > 0 {
			holder = &HolderInfo{
				PID:       rec.Runtime.HolderPID,
				StartTime: rec.Runtime.HolderStart,
				Proto:     rec.Runtime.Proto,
			}
		}
		recKey := key
		if hasRec {
			recKey = rec.RuntimeKey()
		}
		return RuntimeInfo{
			Kind:          kind,
			Presentation:  pres,
			Key:           recKey,
			ProcessOwner:  owner,
			Holder:        holder,
			PinnedMode:    rec.Runtime.PinnedMode,
			RequestedMode: reqMode,
			Authoritative: false,
			Capabilities:  computeCapabilities(kind, owner == "holder", false),
			Source:        "record",
			Reason:        "session process not live (reported from record)",
		}
	}

	// (5) tmux has-session on TmuxSession for records predating the field -> tmux, source:"legacy-probe"
	if hasRec && (rec.Runtime == nil || rec.Runtime.Kind == "") && rec.RuntimeKey() != "" && m != nil && m.tmux != nil {
		if m.tmux.HasSession(ctx, rec.RuntimeKey()) {
			pinned := ""
			if pin, ok := m.PinnedRuntime(rec.RuntimeKey()); ok {
				pinned = string(pin)
			}
			return RuntimeInfo{
				Kind:          "tmux",
				Presentation:  string(ptyevent.PresentationTmux),
				Key:           rec.RuntimeKey(),
				ProcessOwner:  "tmux",
				PinnedMode:    pinned,
				RequestedMode: reqMode,
				Authoritative: false,
				Capabilities:  computeCapabilities("tmux", false, false),
				Source:        "legacy-probe",
			}
		}
	}

	// Unknown with actionable reason
	pres := string(ptyevent.PresentationTmux)
	if hasRec && rec.Kind == "terminal" {
		pres = string(ptyevent.PresentationDirect)
	}
	recKey := sessionID
	if hasRec {
		recKey = rec.RuntimeKey()
	}
	reason := ""
	if !hasRec {
		reason = fmt.Sprintf("session record not found: %s", sessionID)
	} else if rec.Runtime != nil && rec.Runtime.Kind == "direct" {
		reason = fmt.Sprintf("direct session process is dead; restore with 'warden agent restore %s'", sessionID)
	} else {
		reason = fmt.Sprintf("session %s process is dead or missing and no live tmux session exists", sessionID)
	}

	pinned := ""
	if m != nil {
		if pin, ok := m.PinnedRuntime(recKey); ok {
			pinned = string(pin)
		}
	}
	if pinned == "" && hasRec && rec.Runtime != nil {
		pinned = rec.Runtime.PinnedMode
	}

	return RuntimeInfo{
		Kind:          "unknown",
		Presentation:  pres,
		Key:           recKey,
		ProcessOwner:  "none",
		PinnedMode:    pinned,
		RequestedMode: reqMode,
		Authoritative: false,
		Capabilities:  []string{},
		Source:        "none",
		Reason:        reason,
	}
}
