package ptyruntime

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/srjn45/warden/internal/ptyevent"
)

// Controller lease (plan 02 t3, contract §4.1). Plan 01 arbitrates by origin
// (human > operator > warden) but not by client: any number of interactive
// clients could type at once and a client that reconnected after a takeover (or
// after the epoch changed under it) could keep writing. A session now has many
// observers and ONE controller. The controller holds a generation that grows on
// every takeover; the generation space is epoch<<32 | n, so it also grows on
// every epoch change (daemon restart, respawn) and a client fenced out by either
// is rejected the same way.

// Control errors.
var (
	// ErrControllerBusy: another client controls the session and takeover was
	// not requested.
	ErrControllerBusy = errors.New("ptyruntime: another client controls this session")
	// ErrStaleController: the writer's generation is no longer the controller's
	// (it was taken over, released, or the epoch changed).
	ErrStaleController = errors.New("ptyruntime: stale controller generation; input rejected")
	// ErrDuplicateInput: the fenced input id was already applied (a reconnect
	// resent it). The bytes were NOT written again; callers treat it as success.
	ErrDuplicateInput = errors.New("ptyruntime: input already applied")
)

type controller struct {
	clientID string
	gen      uint64
	demoted  chan struct{}
}

// Controller is a held lease. Demoted is closed when another client takes the
// session over (the holder must stop writing and re-attach as an observer).
type Controller struct {
	ClientID   string
	Generation uint64
	Demoted    <-chan struct{}
}

// Fence tags one input write with the lease it was issued under and a
// per-client monotonic id used to drop a resent write after a reconnect.
type Fence struct {
	Generation uint64
	// InputID is monotonic per client; 0 disables de-duplication.
	InputID uint64
}

// Acquire gives clientID the controller lease. Another controller keeps it
// unless takeover is set, in which case it is demoted, the generation bumps and
// the takeover is journaled. Re-acquiring as the current controller returns the
// existing lease (same generation).
func (s *Session) Acquire(clientID string, takeover bool) (Controller, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended || s.closed {
		return Controller{}, ErrExited
	}
	prev := ""
	if s.ctl.demoted != nil {
		if s.ctl.clientID == clientID {
			return Controller{ClientID: clientID, Generation: s.ctl.gen, Demoted: s.ctl.demoted}, nil
		}
		if !takeover {
			return Controller{}, fmt.Errorf("%w (held by %s)", ErrControllerBusy, s.ctl.clientID)
		}
		prev = s.ctl.clientID
		close(s.ctl.demoted)
	}
	s.genSeq++
	s.ctl = controller{clientID: clientID, gen: s.genSeq, demoted: make(chan struct{})}
	detail := fmt.Sprintf("controller gen=%d", s.ctl.gen)
	if prev != "" {
		detail += " takeover from " + prev
	}
	ev := s.event(ptyevent.KindLifecycle)
	ev.Lifecycle = &ptyevent.Lifecycle{Phase: ptyevent.PhaseAttached, ClientID: clientID, Detail: detail,
		Presentation: ptyevent.PresentationDirect}
	_ = s.commit(ev)
	return Controller{ClientID: clientID, Generation: s.ctl.gen, Demoted: s.ctl.demoted}, nil
}

// ReleaseControl gives the lease up (the controller disconnected). A stale
// release (already taken over) is a no-op.
func (s *Session) ReleaseControl(c Controller) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctl.demoted != nil && s.ctl.gen == c.Generation {
		close(s.ctl.demoted)
		s.ctl = controller{}
	}
}

// ControllerInfo reports the current controller ("" when none) and the newest
// generation issued.
func (s *Session) ControllerInfo() (clientID string, gen uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctl.clientID, s.genSeq
}

// checkFence validates a fenced write. Caller holds mu.
func (s *Session) checkFence(w Write) error {
	if w.Fence == nil {
		return nil
	}
	if s.ctl.demoted == nil || s.ctl.gen != w.Fence.Generation || s.ctl.clientID != w.ClientID {
		return ErrStaleController
	}
	if w.Fence.InputID != 0 && w.Fence.InputID <= s.lastInput[w.ClientID] {
		return ErrDuplicateInput
	}
	return nil
}

// inputWindow bounds how much journal tail a re-adoption scans for applied
// fenced input ids: a reconnecting client only ever resends unacknowledged
// input, which is recent.
const inputWindow = 5000

// restoreInputIDs rebuilds the applied-input-id table from the journal so a
// client reconnecting after a daemon restart cannot make the shell run the same
// keystrokes twice. The id is recorded after the bytes reach the PTY, so a
// crash in that instant can still duplicate one write (documented limit).
func (s *Session) restoreInputIDs(last ptyevent.Event) {
	from := uint64(1)
	if last.Seq > inputWindow {
		from = last.Seq - inputWindow
	}
	_ = s.j.Read(from, func(ev ptyevent.Event) error {
		if ev.Seq > last.Seq {
			return errStop
		}
		if ev.Kind == ptyevent.KindInput && ev.Input != nil && ev.Input.InputID > s.lastInput[ev.Input.ClientID] {
			s.lastInput[ev.Input.ClientID] = ev.Input.InputID
		}
		return nil
	})
}

// nudgeRedraw asks the foreground process group to repaint (SIGWINCH). A
// re-adopted session starts with an empty screen model; full-screen programs
// redraw on SIGWINCH even when the size is unchanged, which repopulates both the
// attached clients and the snapshot.
func (s *Session) nudgeRedraw() {
	var pg int
	if err := control(s.master, func(fd int) (err error) {
		pg, err = unix.IoctlGetInt(fd, unix.TIOCGPGRP)
		return err
	}); err == nil && pg > 0 {
		_ = unix.Kill(-pg, unix.SIGWINCH)
	}
}

// LastInputID is the highest fenced input id applied for clientID: a
// reconnecting client resends only what is above it.
func (s *Session) LastInputID(clientID string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastInput[clientID]
}
