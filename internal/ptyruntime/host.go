package ptyruntime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

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

	mu       sync.Mutex
	sessions map[string]*hosted
}

type hosted struct {
	s *Session
	j *Journal
}

var _ tmuxproc.Host = (*Host)(nil)

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
func (h *Host) NewSession(_ context.Context, name, cwd string, env ...string) error {
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
	s, err := Start(Config{Journal: j, Argv: h.shell(), Dir: cwd, Env: append(full, env...),
		Cols: h.Cols, Rows: h.Rows, Options: h.Options})
	if err != nil {
		_ = j.Close()
		return err
	}
	h.sessions[name] = &hosted{s: s, j: j}
	return nil
}

// release closes a session and its journal. Caller holds mu.
func (h *Host) release(name string, hs *hosted) {
	_ = hs.s.Close()
	_ = hs.j.Close()
	delete(h.sessions, name)
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

// Close closes every hosted session and journal.
func (h *Host) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for name, hs := range h.sessions {
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
