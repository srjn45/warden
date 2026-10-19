package ptyhost

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyruntime"
)

// The §6 attach protocol over one WebSocket.
//
// Client → server:
//   - text {"type":"hello","cursor":{session_id,epoch,seq}?,"cols","rows","mode":"interactive"|"observe","client_id"?,"takeover"?}
//     opens (or, sent again, restarts) the stream from the cursor. A hello
//     without a cursor resumes from the client's last ack when client_id has
//     one, else it is a fresh attach. An interactive hello asks for the
//     CONTROLLER lease (one per session; plan 02 §4.1): it is granted when free
//     or when takeover is set (the previous controller is demoted); otherwise the
//     client is an interactive viewer (raw feed, no input, no resize).
//   - text {"type":"takeover"} — take the controller lease now.
//   - text {"type":"input","gen":N,"id":M,"data":"<base64>"} — fenced human
//     input: rejected unless gen is the controller generation this socket holds;
//     id (monotonic per client_id) makes a resend after a reconnect a no-op.
//   - text {"type":"resize","cols","rows"} (the legacy {"cols","rows"} body is
//     accepted too) — interactive direct sessions only.
//   - text {"type":"ack","seq"} — a cursor checkpoint stored per client_id.
//   - binary — human keystrokes (origin=human, client_id = this socket's),
//     controller only; a demoted/stale socket's bytes are dropped.
//
// Server → client:
//   - text {"type":"plan",...} once per hello: how the cursor was resolved
//     (mode tail|replay|snapshot), head (the replay/live boundary),
//     presentation, and whether the stream is interactive.
//   - text {"type":"event","event":{…}} — every journaled (redacted) event, in
//     seq order. For mode snapshot the first one is the snapshot marker the
//     client resets its terminal model to.
//   - binary (interactive only) — the live, UNREDACTED PTY bytes. An
//     interactive client renders output events with seq <= head from the
//     (redacted) event data and everything after from binary frames; events
//     past head then only advance its cursor.
//   - text {"type":"reject","reason"} — the cursor cannot be served; discard
//     state and send a fresh hello.
//   - text {"type":"resync","reason"} — the stream fell behind; re-send hello
//     with the last cursor.
//   - text {"type":"demoted","reason","holder"} — another client took the
//     controller lease; this socket is now a viewer.
//   - text {"type":"controller","generation":N} — answers takeover.
//   - text {"type":"rejected","reason"} — an input frame was refused (stale
//     generation); the client must re-take control before typing.

// ClientFrame is a client text frame.
type ClientFrame struct {
	Type     string           `json:"type"`
	Cursor   *ptyevent.Cursor `json:"cursor,omitempty"`
	Cols     uint16           `json:"cols,omitempty"`
	Rows     uint16           `json:"rows,omitempty"`
	Mode     string           `json:"mode,omitempty"`
	ClientID string           `json:"client_id,omitempty"`
	Seq      uint64           `json:"seq,omitempty"`
	Takeover bool             `json:"takeover,omitempty"`
	// Gen / ID / Data are a fenced input frame.
	Gen  uint64 `json:"gen,omitempty"`
	ID   uint64 `json:"id,omitempty"`
	Data []byte `json:"data,omitempty"`
}

// PlanFrame answers a hello.
type PlanFrame struct {
	Mode         ptyevent.ResumeMode   `json:"mode"`
	FromSeq      uint64                `json:"from_seq,omitempty"`
	Head         uint64                `json:"head"`
	SessionID    string                `json:"session_id"`
	Epoch        uint64                `json:"epoch"`
	Presentation ptyevent.Presentation `json:"presentation"`
	Interactive  bool                  `json:"interactive"`
	ClientID     string                `json:"client_id"`
	Reason       string                `json:"reason,omitempty"`
	// Fidelity labels the replay plane. It is additive: clients written before
	// this field ignore it. Journal replay/snapshots are redacted text, while a
	// tail plan renders only authenticated live bytes after Head.
	Fidelity string `json:"fidelity,omitempty"`
	// Controller: this socket holds the controller lease (Generation is its
	// fence). Holder names the current controller when it does not.
	Controller bool   `json:"controller,omitempty"`
	Generation uint64 `json:"generation,omitempty"`
	Holder     string `json:"holder,omitempty"`
	// LastInputID is the highest fenced input id already applied for this
	// client_id: a reconnecting client resends only what is above it.
	LastInputID uint64 `json:"last_input_id,omitempty"`
}

// ServerFrame is a server text frame.
type ServerFrame struct {
	Type   string          `json:"type"`
	Plan   *PlanFrame      `json:"plan,omitempty"`
	Event  *ptyevent.Event `json:"event,omitempty"`
	Reason string          `json:"reason,omitempty"`
	Holder string          `json:"holder,omitempty"`
	// Generation answers a takeover.
	Generation uint64 `json:"generation,omitempty"`
}

// Hello modes.
const (
	ModeInteractive = "interactive"
	ModeObserve     = "observe"
)

// helloTimeout bounds how long a fresh socket may stay silent before hello.
var helloTimeout = 30 * time.Second

// acks remembers each client's last acknowledged cursor, per session.
type acks struct {
	mu sync.Mutex
	m  map[[2]string]ptyevent.Cursor
}

func (a *acks) set(name, client string, c ptyevent.Cursor) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.m == nil {
		a.m = map[[2]string]ptyevent.Cursor{}
	}
	a.m[[2]string{name, client}] = c
}

func (a *acks) get(name, client string) (ptyevent.Cursor, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.m[[2]string{name, client}]
	return c, ok
}

// Serve speaks the §6 protocol for t over conn until the client leaves, ctx
// ends, or the session closes. clientID names this socket (input and resize
// origin); a hello may override it to resume its own acked cursor.
func (m *Manager) Serve(ctx context.Context, conn *websocket.Conn, t Target, clientID string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn.SetReadLimit(1 << 20)
	var wmu sync.Mutex // one writer at a time on conn
	writeText := func(f ServerFrame) error {
		b, err := json.Marshal(f)
		if err != nil {
			return err
		}
		wmu.Lock()
		defer wmu.Unlock()
		return conn.Write(ctx, websocket.MessageText, b)
	}
	writeBinary := func(b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return conn.Write(ctx, websocket.MessageBinary, b)
	}

	var (
		streamCancel context.CancelFunc
		streamDone   chan struct{}
		interactive  bool
		held         atomic.Pointer[ptyruntime.Controller] // the lease this socket holds (nil = viewer)
	)
	releaseControl := func() {
		if c := held.Swap(nil); c != nil && t.Direct != nil {
			t.Direct.ReleaseControl(*c)
		}
	}
	defer releaseControl()
	// watchDemotion tells the client when another client takes the lease.
	watchDemotion := func(c ptyruntime.Controller) {
		go func() {
			select {
			case <-c.Demoted:
				if cur := held.Load(); cur != nil && cur.Generation == c.Generation {
					held.Store(nil)
					holder, _ := t.Direct.ControllerInfo()
					_ = writeText(ServerFrame{Type: "demoted", Reason: "another client took control", Holder: holder})
				}
			case <-ctx.Done():
			}
		}()
	}
	stopStream := func() {
		if streamCancel != nil {
			streamCancel()
			<-streamDone
			streamCancel = nil
		}
	}
	defer stopStream()

	hctx, hcancel := context.WithTimeout(ctx, helloTimeout)
	defer hcancel()
	first := true
	for {
		rctx := ctx
		if first {
			rctx = hctx
		}
		typ, data, err := conn.Read(rctx)
		if first {
			hcancel()
		}
		if err != nil {
			if ctx.Err() != nil || websocket.CloseStatus(err) != -1 {
				return nil
			}
			return err
		}
		if typ == websocket.MessageBinary {
			if first {
				return writeText(ServerFrame{Type: "reject", Reason: "expected hello"})
			}
			if c := held.Load(); interactive && t.Direct != nil && c != nil {
				// Keystrokes ride the lease this socket holds; once it is taken over
				// (or released) the fence rejects them — dropped, never delivered.
				_, err := t.Direct.Input(ptyruntime.Write{Origin: ptyevent.OriginHuman, ClientID: clientID, Data: data,
					Fence: &ptyruntime.Fence{Generation: c.Generation}})
				if errors.Is(err, ptyruntime.ErrStaleController) {
					held.Store(nil)
				}
			}
			continue
		}
		var f ClientFrame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		if f.Type == "" && f.Cols > 0 && f.Rows > 0 {
			f.Type = "resize" // legacy attach body
		}
		if first && f.Type != "hello" {
			return writeText(ServerFrame{Type: "reject", Reason: "expected hello"})
		}
		switch f.Type {
		case "hello":
			first = false
			stopStream()
			if f.ClientID != "" {
				clientID = f.ClientID
			}
			cur := ptyevent.Cursor{}
			if f.Cursor != nil {
				cur = *f.Cursor
			} else if c, ok := m.acks.get(t.Name, clientID); ok {
				cur = c
			}
			interactive = f.Mode == ModeInteractive && t.Direct != nil
			releaseControl()
			ci := ctlInfo{}
			if interactive {
				switch c, err := t.Direct.Acquire(clientID, f.Takeover); {
				case err == nil:
					held.Store(&c)
					watchDemotion(c)
					ci.controller, ci.gen = true, c.Generation
					if f.Cols > 0 && f.Rows > 0 {
						_ = t.Direct.Resize(f.Cols, f.Rows, ptyevent.OriginHuman, clientID)
					}
				case errors.Is(err, ptyruntime.ErrControllerBusy):
					ci.holder, ci.gen = t.Direct.ControllerInfo()
				default:
					// The shell is gone: serve the stream as a viewer; it reports the exit.
					interactive = false
				}
				ci.lastInput = t.Direct.LastInputID(clientID)
			}
			sctx, sc := context.WithCancel(ctx)
			streamCancel, streamDone = sc, make(chan struct{})
			go func(done chan struct{}, interactive bool, cid string) {
				defer close(done)
				if err := m.stream(sctx, t, cur, interactive, cid, ci, writeText, writeBinary); err != nil {
					cancel() // socket write failed or session closed: end Serve
				}
			}(streamDone, interactive, clientID)
		case "takeover":
			if interactive && t.Direct != nil {
				releaseControl()
				if c, err := t.Direct.Acquire(clientID, true); err == nil {
					held.Store(&c)
					watchDemotion(c)
					_ = writeText(ServerFrame{Type: "controller", Generation: c.Generation})
				}
			}
		case "input":
			c := held.Load()
			if !interactive || t.Direct == nil || c == nil || f.Gen != c.Generation {
				_ = writeText(ServerFrame{Type: "rejected", Reason: "stale controller generation; take control first"})
				continue
			}
			_, err := t.Direct.Input(ptyruntime.Write{Origin: ptyevent.OriginHuman, ClientID: clientID, Data: f.Data,
				Fence: &ptyruntime.Fence{Generation: f.Gen, InputID: f.ID}})
			if errors.Is(err, ptyruntime.ErrStaleController) {
				held.Store(nil)
				_ = writeText(ServerFrame{Type: "rejected", Reason: err.Error()})
			}
		case "resize":
			// Resize follows the controller (last controller wins); a viewer's
			// size is ignored so it cannot reflow someone else's session.
			if interactive && t.Direct != nil && held.Load() != nil && f.Cols > 0 && f.Rows > 0 {
				_ = t.Direct.Resize(f.Cols, f.Rows, ptyevent.OriginHuman, clientID)
			}
		case "ack":
			if j := t.Stream.Journal(); f.Seq > 0 && f.Seq <= j.Last().Seq {
				m.acks.set(t.Name, clientID, ptyevent.Cursor{SessionID: j.SessionID(), Seq: f.Seq})
			}
		}
	}
}

// ctlInfo is what a hello learned about the controller lease.
type ctlInfo struct {
	controller bool
	gen        uint64
	holder     string
	lastInput  uint64
}

// errStreamEnded ends Serve after the session closed (or the socket broke).
var errStreamEnded = errors.New("ptyhost: stream ended")

// stream sends one hello's worth of stream: plan, journal replay, then live
// events (and, interactive, the raw live bytes after the replay boundary). It
// returns nil when ctx ends (a new hello or the client left), errStreamEnded
// when the session closed or a write failed.
func (m *Manager) stream(ctx context.Context, t Target, cur ptyevent.Cursor, interactive bool, clientID string, ci ctlInfo,
	writeText func(ServerFrame) error, writeBinary func([]byte) error) error {
	// Raw attach first, so no live byte after the replay boundary is missed;
	// raw frames are held back (in the attach buffer) until the replay is out.
	var raw *ptyruntime.RawSubscription
	if interactive {
		// Registered at a quiescent cut (journal and screen caught up), so the
		// replay and the raw feed meet without a hole or a repeat.
		r, _, _, _, err := t.Direct.AttachQuiesced(clientID)
		if err != nil {
			_ = writeText(ServerFrame{Type: "reject", Reason: err.Error()})
			return errStreamEnded
		}
		raw = r
		defer raw.Close()
	}
	sub := t.Stream.Subscribe(cur)
	defer sub.Close()
	fidelity := "exact-live"
	if sub.Plan.Mode == ptyevent.ResumeSnapshot || sub.Plan.Mode == ptyevent.ResumeReplay {
		fidelity = "redacted-text"
	}
	plan := &PlanFrame{Mode: sub.Plan.Mode, FromSeq: sub.Plan.FromSeq, Head: sub.Head, SessionID: t.Stream.Journal().SessionID(),
		Epoch: t.Stream.Epoch(), Presentation: t.Presentation, Interactive: interactive, ClientID: clientID, Reason: sub.Plan.Reason,
		Controller: ci.controller, Generation: ci.gen, Holder: ci.holder, LastInputID: ci.lastInput, Fidelity: fidelity}
	if sub.Plan.Mode == ptyevent.ResumeReject {
		_ = writeText(ServerFrame{Type: "reject", Plan: plan, Reason: sub.Plan.Reason})
		return nil // the client may send a fresh hello
	}
	if err := writeText(ServerFrame{Type: "plan", Plan: plan}); err != nil {
		return errStreamEnded
	}
	replaying := sub.Plan.Mode != ptyevent.ResumeTail && sub.Head >= sub.Plan.FromSeq
	var rawC <-chan []byte
	if raw != nil && !replaying {
		rawC = raw.C
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-sub.C:
			if !ok {
				switch err := sub.Err(); {
				case errors.Is(err, ptyruntime.ErrResync):
					_ = writeText(ServerFrame{Type: "resync", Reason: err.Error()})
					return nil
				case errors.Is(err, ptyruntime.ErrRejected):
					_ = writeText(ServerFrame{Type: "reject", Reason: err.Error()})
					return nil
				}
				return errStreamEnded
			}
			if err := writeText(ServerFrame{Type: "event", Event: &ev}); err != nil {
				return errStreamEnded
			}
			if replaying && ev.Seq >= sub.Head {
				replaying = false
				if raw != nil {
					rawC = raw.C
				}
			}
		case b, ok := <-rawC:
			if !ok {
				if !raw.Lagged() {
					return errStreamEnded // the session closed
				}
				_ = writeText(ServerFrame{Type: "resync", Reason: "interactive feed fell behind"})
				return nil
			}
			if err := writeBinary(b); err != nil {
				return errStreamEnded
			}
		}
	}
}

// BridgeLegacy serves today's raw attach protocol (binary keystrokes out and
// in, text {cols,rows} resize) for a direct session, so existing attach
// clients keep working when a terminal is hosted on the PTY runtime. The
// client first gets the current (redacted) screen, then live bytes; the two are
// cut at a quiescent point so nothing is lost or repeated between them, and a
// client that falls behind is re-synced with a fresh screen instead of being
// dropped (G10).
//
// A legacy client has no controller frames, so it is the compat "human": it
// takes the controller lease when it connects (demoting a fenced client, human
// precedence) and its keystrokes are written unfenced.
func BridgeLegacy(ctx context.Context, conn *websocket.Conn, s *ptyruntime.Session, clientID string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn.SetReadLimit(1 << 20)
	var wmu sync.Mutex
	write := func(b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return conn.Write(ctx, websocket.MessageBinary, b)
	}
	if c, err := s.Acquire(clientID, true); err == nil {
		defer s.ReleaseControl(c)
	}
	raw, scr, _, _, err := s.AttachQuiesced(clientID)
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "attach failed")
		return
	}
	if scr != "" {
		_ = write(ptyruntime.ScreenBytes([]byte(scr)))
	}
	go func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				raw.Close()
				return
			case b, ok := <-raw.C:
				if !ok {
					if !raw.Lagged() {
						return // the session closed
					}
					// Fell behind: resync from a fresh cut, never drop the client.
					raw.Close()
					nr, scr, _, _, err := s.AttachQuiesced(clientID)
					if err != nil {
						return
					}
					raw = nr
					if write(ptyruntime.ScreenBytes([]byte(scr))) != nil {
						return
					}
					continue
				}
				if write(b) != nil {
					raw.Close()
					return
				}
			}
		}
	}()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		switch typ {
		case websocket.MessageText:
			var f ClientFrame
			if json.Unmarshal(data, &f) == nil && f.Cols > 0 && f.Rows > 0 {
				_ = s.Resize(f.Cols, f.Rows, ptyevent.OriginHuman, clientID)
			}
		case websocket.MessageBinary:
			if _, err := s.Input(ptyruntime.Write{Origin: ptyevent.OriginHuman, ClientID: clientID, Data: data}); errors.Is(err, ptyruntime.ErrExited) {
				return
			}
		}
	}
}
