package ptyruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/tmuxproc"
)

// Host adapts warden-owned PTY sessions to tmuxproc.Host, keyed by the same
// name lifecycle passes to tmux, so the runtime can stand in for tmux behind
// `runtime.pty` (M1) without lifecycle knowing. Like `tmux new-session`,
// NewSession starts a shell; the caller then types its launch line with
// SendKeys (a direct PTY has no 1024-byte MAX_CANON concern for argv, but the
// typed line keeps lifecycle unchanged).
type Host struct {
	// JournalDir holds one journal directory per session name.
	JournalDir string
	// Shell is the command NewSession starts (default: $SHELL, else /bin/sh).
	Shell     []string
	Cols      uint16
	Rows      uint16
	Retention Retention
	Options   Options
	// Launcher, when set, hosts every new session in an independent PTY holder
	// process (it owns the master and the child) so the session survives this
	// process; nil keeps the process a child of this one.
	Launcher Launcher

	mu       sync.Mutex
	sessions map[string]*hosted
}

type hosted struct {
	s *Session
	j *Journal
}

// Launcher starts and re-adopts processes owned by an independent holder (see
// internal/ptyholder; the interface keeps ptyruntime free of that dependency).
type Launcher interface {
	// Launch starts argv under a holder and adopts it.
	Launch(ctx context.Context, name string, argv []string, cwd string, env []string, cols, rows uint16) (*Adoption, error)
	// Adopt authenticates to name's existing holder and adopts the live process.
	Adopt(ctx context.Context, name string) (*Adoption, error)
	// Has reports whether a holder record exists for name (it may be dead).
	Has(name string) bool
	// Release tells the holder the session is over so it exits and forgets it.
	Release(ctx context.Context, name string)
}

var _ tmuxproc.Host = (*Host)(nil)

// ErrNoHolder: the session has no holder record, so there is nothing to adopt.
var ErrNoHolder = errors.New("ptyruntime: no PTY holder record for session")

func (h *Host) shell() []string {
	if len(h.Shell) > 0 {
		return h.Shell
	}
	if sh := os.Getenv("SHELL"); sh != "" {
		return []string{sh}
	}
	return []string{"/bin/sh"}
}

// NewSession implements tmuxproc.Host.
func (h *Host) NewSession(ctx context.Context, name, cwd string, env ...string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessions == nil {
		h.sessions = map[string]*hosted{}
	}
	if old, ok := h.sessions[name]; ok {
		if old.s.Alive() {
			return fmt.Errorf("ptyruntime: duplicate session: %s", name)
		}
		h.release(name, old)
	}
	j, err := OpenJournal(filepath.Join(h.JournalDir, name), name, h.Retention)
	if err != nil {
		return err
	}
	full := append(os.Environ(), "WARDEN_SESSION_ID="+name, "AGENTCTL_SESSION_ID="+name)
	cfg := Config{Journal: j, Argv: h.shell(), Dir: cwd, Env: append(full, env...),
		Cols: h.Cols, Rows: h.Rows, Options: h.Options}
	if h.Launcher != nil {
		ad, err := h.Launcher.Launch(ctx, name, cfg.Argv, cwd, cfg.Env, cfg.Cols, cfg.Rows)
		if err != nil {
			_ = j.Close()
			return err
		}
		cfg.Adopt = ad
	}
	s, err := Start(cfg)
	if err != nil {
		_ = j.Close()
		if h.Launcher != nil {
			h.Launcher.Release(ctx, name)
		}
		return err
	}
	h.sessions[name] = &hosted{s: s, j: j}
	return nil
}

// Adopt re-attaches to name's live holder-hosted process after this process
// restarted (or crashed): same process, same journal, a new epoch with
// lifecycle{recovered}. It never starts a process. Errors are the holder's
// (gone, refused, identity) so the caller can report cause and next step.
func (h *Host) Adopt(ctx context.Context, name string) error {
	if h.Launcher == nil || !h.Launcher.Has(name) {
		return ErrNoHolder
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessions == nil {
		h.sessions = map[string]*hosted{}
	}
	if old, ok := h.sessions[name]; ok && old.s.Alive() {
		return nil // already adopted: idempotent
	}
	ad, err := h.Launcher.Adopt(ctx, name)
	if err != nil {
		return err
	}
	ad.Recovered = true
	j, err := OpenJournal(filepath.Join(h.JournalDir, name), name, h.Retention)
	if err != nil {
		_ = ad.Master.Close()
		return err
	}
	s, err := Start(Config{Journal: j, Options: h.Options, Adopt: ad})
	if err != nil {
		_ = j.Close()
		return err
	}
	h.sessions[name] = &hosted{s: s, j: j}
	return nil
}

// HolderHosted reports whether name has a holder record.
func (h *Host) HolderHosted(name string) bool { return h.Launcher != nil && h.Launcher.Has(name) }

// MarkLost journals that name's process was lost without a recorded exit (the
// holder is gone or refused adoption): a crashed lifecycle event carrying detail
// (cause and next step), so a client sees why instead of a silent relaunch.
func (h *Host) MarkLost(name, detail string) error {
	return MarkLost(filepath.Join(h.JournalDir, name), name, h.Retention, ptyevent.PresentationDirect, detail)
}

// MarkLost appends lifecycle{crashed, detail} to the journal at dir when its
// newest epoch has not ended. It is how a process warden can no longer reach
// (or refused to adopt) is reported instead of silently replaced.
func MarkLost(dir, name string, ret Retention, pres ptyevent.Presentation, detail string) error {
	j, err := OpenJournal(dir, name, ret)
	if err != nil {
		return err
	}
	defer j.Close()
	last := j.Last()
	if last.Seq == 0 || j.Ended() {
		return nil
	}
	ev := ptyevent.Event{V: ptyevent.SchemaVersion, SessionID: name, Epoch: last.Epoch, Seq: j.NextSeq(),
		MonoNS: last.MonoNS, Wall: time.Now(), Kind: ptyevent.KindLifecycle, Redacted: true,
		Lifecycle: &ptyevent.Lifecycle{Phase: ptyevent.PhaseCrashed, Detail: detail, Presentation: pres}}
	_, err = j.Append(ev)
	return err
}

// release closes a session and its journal. Caller holds mu.
func (h *Host) release(name string, hs *hosted) {
	_ = hs.s.Close()
	_ = hs.j.Close()
	delete(h.sessions, name)
	if h.Launcher != nil {
		h.Launcher.Release(context.Background(), name)
	}
}

// Session returns the hosted session by name (nil if none).
func (h *Host) Session(name string) *Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	if hs, ok := h.sessions[name]; ok {
		return hs.s
	}
	return nil
}

// KillSession implements tmuxproc.Host. The journal stays on disk.
func (h *Host) KillSession(_ context.Context, name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if hs, ok := h.sessions[name]; ok {
		h.release(name, hs)
	}
	return nil
}

// HasSession implements tmuxproc.Host: the session exists and its process
// tree is alive.
func (h *Host) HasSession(_ context.Context, name string) bool {
	s := h.Session(name)
	return s != nil && s.Alive()
}

// CapturePane implements tmuxproc.Host from the (redacted) screen model.
func (h *Host) CapturePane(_ context.Context, name string) (string, error) {
	s := h.Session(name)
	if s == nil {
		return "", fmt.Errorf("ptyruntime: can't find session: %s", name)
	}
	return s.Screen(), nil
}

// SendKeys implements tmuxproc.Host with tmux send-keys semantics: each key
// that names a key (Enter, Escape, C-c, M-x, Up, …) sends that key, anything
// else is typed literally; a leading "-l" makes every key literal. The write
// is operator input: it waits while a human is mid-line.
func (h *Host) SendKeys(ctx context.Context, name string, keys ...string) error {
	s := h.Session(name)
	if s == nil {
		return fmt.Errorf("ptyruntime: can't find session: %s", name)
	}
	return s.WriteWait(ctx, Write{Origin: ptyevent.OriginOperator, Data: KeyBytes(keys...)})
}

// Close closes every hosted session and journal. A holder-hosted session is
// DETACHED, not ended: its process and holder keep running and the next daemon
// adopts them. Only KillSession ends a process.
func (h *Host) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for name, hs := range h.sessions {
		if hs.s.adopt != nil {
			_ = hs.s.Detach()
			_ = hs.j.Close()
			delete(h.sessions, name)
			continue
		}
		h.release(name, hs)
	}
}

var namedKeys = map[string]string{
	"Enter": "\r", "Escape": "\x1b", "Tab": "\t", "BTab": "\x1b[Z", "BSpace": "\x7f", "Space": " ",
	"Up": "\x1b[A", "Down": "\x1b[B", "Right": "\x1b[C", "Left": "\x1b[D",
	"Home": "\x1b[H", "End": "\x1b[F", "PageUp": "\x1b[5~", "PPage": "\x1b[5~",
	"PageDown": "\x1b[6~", "NPage": "\x1b[6~", "DC": "\x1b[3~", "Delete": "\x1b[3~", "IC": "\x1b[2~",
}

// KeyBytes translates tmux send-keys arguments into the bytes a terminal
// would send.
func KeyBytes(keys ...string) []byte {
	literal := false
	if len(keys) > 0 && keys[0] == "-l" {
		literal, keys = true, keys[1:]
	}
	var out []byte
	for _, k := range keys {
		if !literal {
			if b, ok := keyByte(k); ok {
				out = append(out, b...)
				continue
			}
		}
		out = append(out, k...)
	}
	return out
}

func keyByte(k string) ([]byte, bool) {
	if v, ok := namedKeys[k]; ok {
		return []byte(v), true
	}
	if rest, ok := strings.CutPrefix(k, "M-"); ok && rest != "" {
		if b, ok := keyByte(rest); ok {
			return append([]byte{0x1b}, b...), true
		}
		if len(rest) == 1 {
			return []byte{0x1b, rest[0]}, true
		}
		return nil, false
	}
	if rest, ok := strings.CutPrefix(k, "C-"); ok && len(rest) == 1 {
		c := rest[0]
		switch {
		case c >= 'a' && c <= 'z':
			return []byte{c - 'a' + 1}, true
		case c >= '@' && c <= '_':
			return []byte{c - '@'}, true
		case c == '?':
			return []byte{0x7f}, true
		}
	}
	return nil, false
}
