//go:build linux || darwin

package ptyattach

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyhost"
	"github.com/srjn45/warden/internal/ptyruntime"
)

// DefaultDetachKey is Ctrl-]. Followed by '.' it detaches, by 't' it takes the
// controller lease, and doubled it sends a literal Ctrl-].
const DefaultDetachKey byte = 0x1d

// ErrNotDirect: the session is hosted by tmux, not by a warden PTY; attach
// with tmux instead. Nothing was rendered and nothing was sent.
var ErrNotDirect = errors.New("ptyattach: session is not hosted on a warden PTY")

// ErrGaveUp: the connection could not be re-established.
var ErrGaveUp = errors.New("ptyattach: could not reconnect to the daemon")

// HandshakeError is a refused WebSocket upgrade (the daemon answered HTTP).
type HandshakeError struct{ Status int }

func (e *HandshakeError) Error() string {
	return fmt.Sprintf("ptyattach: daemon refused the stream (HTTP %d)", e.Status)
}

// Options configures Run.
type Options struct {
	// URL is the daemon's /api/v1/sessions/<id>/pty WebSocket URL; Token the
	// bearer token (may be empty).
	URL, Token string
	// In is the local terminal (nil: no input, view only). Out receives the
	// session's bytes. Status receives one-line notices (default io.Discard).
	In     *os.File
	Out    io.Writer
	Status io.Writer
	// ClientID names this client across reconnects (default "attach-<pid>").
	ClientID string
	// Observe attaches read-only: no controller lease, no input.
	Observe bool
	// Takeover demotes the current controller on attach (what a person who
	// asked to attach wants); without it a busy session attaches as a viewer.
	Takeover  bool
	DetachKey byte
	// MaxReconnects bounds consecutive failed re-dials (default 8).
	MaxReconnects int
	// Backoff is the wait before re-dial n (default 250ms·2^n, capped at 5s).
	Backoff func(n int) time.Duration
	// Signals installs SIGTERM/SIGHUP/SIGINT (→ detach, terminal restored) and
	// SIGWINCH (→ resize) handling. The CLI sets it; tests drive Winch instead.
	Signals bool
	// Winch delivers resize notifications (tests); Signals adds SIGWINCH to it.
	Winch <-chan struct{}
	// Dial overrides the WebSocket dial (tests).
	Dial func(ctx context.Context, url string, h http.Header) (*websocket.Conn, error)
}

// Result says how the attach ended.
type Result struct {
	// Reason: "detached", "exited", "closed" (the daemon ended the stream).
	Reason string
	// ExitCode / Signal describe the process when Reason is "exited".
	ExitCode *int
	Signal   string
}

const pendingCap = 4096

type pendingInput struct {
	id   uint64
	data []byte
}

type state struct {
	mu      sync.Mutex
	cursor  ptyevent.Cursor
	plan    ptyhost.PlanFrame
	gen     uint64
	control bool
	nextID  uint64
	pending []pendingInput
	wake    chan struct{}
}

// Run attaches until the user detaches, the process ends, the context is
// cancelled or the daemon stays unreachable. The local terminal's modes are
// restored on every return path.
func Run(ctx context.Context, o Options) (res Result, err error) {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Status == nil {
		o.Status = io.Discard
	}
	if o.ClientID == "" {
		o.ClientID = fmt.Sprintf("attach-%d", os.Getpid())
	}
	if o.DetachKey == 0 {
		o.DetachKey = detachKeyFromEnv(os.Getenv("WARDEN_ATTACH_DETACH_KEY"))
	}
	if o.MaxReconnects <= 0 {
		o.MaxReconnects = 8
	}
	if o.Backoff == nil {
		o.Backoff = func(n int) time.Duration {
			d := 250 * time.Millisecond << min(n, 5)
			return min(d, 5*time.Second)
		}
	}
	if o.Dial == nil {
		o.Dial = func(ctx context.Context, url string, h http.Header) (*websocket.Conn, error) {
			c, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: h})
			if err != nil && resp != nil {
				return nil, &HandshakeError{Status: resp.StatusCode}
			}
			return c, err
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if o.In != nil && IsTerminal(o.In) {
		restore, rerr := MakeRaw(o.In)
		if rerr != nil {
			return res, rerr
		}
		defer restore()
	}
	winch := make(chan struct{}, 1)
	if o.Winch != nil {
		go func() {
			for {
				select {
				case <-o.Winch:
					select {
					case winch <- struct{}{}:
					default:
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	if o.Signals {
		sigs := make(chan os.Signal, 4)
		signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT, syscall.SIGWINCH)
		defer signal.Stop(sigs)
		go func() {
			for {
				select {
				case s := <-sigs:
					if s == syscall.SIGWINCH {
						select {
						case winch <- struct{}{}:
						default:
						}
						continue
					}
					cancel() // restore the terminal (deferred above) and leave
					return
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	st := &state{wake: make(chan struct{}, 1), nextID: 1}
	var detach = make(chan struct{})
	var detachOnce sync.Once
	doDetach := func() { detachOnce.Do(func() { close(detach) }) }
	takeover := make(chan struct{}, 1)
	if o.In != nil && !o.Observe {
		go readKeys(ctx, o, st, doDetach, takeover)
	}

	failures := 0
	for {
		r, done, cerr := connect(ctx, o, st, winch, detach, takeover)
		if done {
			return r, cerr
		}
		if ctx.Err() != nil {
			return Result{Reason: "detached"}, nil
		}
		select {
		case <-detach:
			return Result{Reason: "detached"}, nil
		default:
		}
		if cerr != nil && errors.Is(cerr, ErrNotDirect) {
			return r, cerr
		}
		var he *HandshakeError
		if errors.As(cerr, &he) {
			switch he.Status {
			case http.StatusConflict, http.StatusNotFound: // no PTY journal: a tmux-hosted session
				return Result{}, fmt.Errorf("%w: %v", ErrNotDirect, he)
			case http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest:
				return Result{Reason: "closed"}, he
			}
		}
		failures++
		if failures > o.MaxReconnects {
			return Result{Reason: "closed"}, fmt.Errorf("%w: %v", ErrGaveUp, cerr)
		}
		fmt.Fprintf(o.Status, "\r\n[warden] connection lost (%v); reconnecting…\r\n", cerr)
		select {
		case <-time.After(o.Backoff(failures - 1)):
		case <-ctx.Done():
			return Result{Reason: "detached"}, nil
		case <-detach:
			return Result{Reason: "detached"}, nil
		}
	}
}

// detachKeyFromEnv accepts one literal byte or caret notation (for example
// "^]" for Ctrl-]). Invalid values deliberately retain the safe default.
func detachKeyFromEnv(v string) byte {
	if len(v) == 1 {
		return v[0]
	}
	if len(v) == 2 && v[0] == '^' {
		return v[1] & 0x1f
	}
	return DefaultDetachKey
}

// readKeys forwards the user's keystrokes as fenced input, handling the detach
// escape (Ctrl-] then '.', 't' or Ctrl-]).
func readKeys(ctx context.Context, o Options, st *state, detach func(), takeover chan<- struct{}) {
	buf := make([]byte, 4096)
	esc := false
	for {
		n, err := o.In.Read(buf)
		if n > 0 {
			var out []byte
			for _, b := range buf[:n] {
				if esc {
					esc = false
					switch b {
					case '.':
						detach()
						return
					case 't':
						select {
						case takeover <- struct{}{}:
						default:
						}
						continue
					case o.DetachKey:
						out = append(out, b)
						continue
					default:
						out = append(out, o.DetachKey, b)
						continue
					}
				}
				if b == o.DetachKey {
					esc = true
					continue
				}
				out = append(out, b)
			}
			if len(out) > 0 {
				st.addInput(out)
			}
		}
		if err != nil || ctx.Err() != nil {
			return
		}
	}
}

func (st *state) addInput(data []byte) {
	st.mu.Lock()
	st.pending = append(st.pending, pendingInput{id: st.nextID, data: append([]byte(nil), data...)})
	st.nextID++
	if len(st.pending) > pendingCap {
		st.pending = st.pending[len(st.pending)-pendingCap:]
	}
	st.mu.Unlock()
	select {
	case st.wake <- struct{}{}:
	default:
	}
}

func (st *state) dropThrough(id uint64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	keep := st.pending[:0]
	for _, p := range st.pending {
		if p.id > id {
			keep = append(keep, p)
		}
	}
	st.pending = keep
}

// connect runs one connection. done is true when the attach is over for good.
func connect(ctx context.Context, o Options, st *state, winch <-chan struct{}, detach <-chan struct{}, takeover <-chan struct{}) (res Result, done bool, err error) {
	hdr := http.Header{}
	if o.Token != "" {
		hdr.Set("Authorization", "Bearer "+o.Token)
	}
	dctx, dcancel := context.WithTimeout(ctx, 15*time.Second)
	conn, err := o.Dial(dctx, o.URL, hdr)
	dcancel()
	if err != nil {
		return Result{}, false, err
	}
	conn.SetReadLimit(8 << 20)
	defer conn.CloseNow()
	cctx, ccancel := context.WithCancel(ctx)
	defer ccancel()

	var wmu sync.Mutex
	send := func(f ptyhost.ClientFrame) error {
		b, _ := json.Marshal(f)
		wmu.Lock()
		defer wmu.Unlock()
		return conn.Write(cctx, websocket.MessageText, b)
	}

	mode := ptyhost.ModeInteractive
	if o.Observe || o.In == nil {
		mode = ptyhost.ModeObserve
	}
	hello := ptyhost.ClientFrame{Type: "hello", Mode: mode, ClientID: o.ClientID, Takeover: o.Takeover}
	if o.In != nil {
		if c, r, e := Size(o.In); e == nil {
			hello.Cols, hello.Rows = c, r
		}
	}
	st.mu.Lock()
	if st.cursor.Seq != 0 {
		c := st.cursor
		hello.Cursor = &c
	}
	st.mu.Unlock()
	if err := send(hello); err != nil {
		return Result{}, false, err
	}

	type outcome struct {
		res  Result
		done bool
		err  error
	}
	end := make(chan outcome, 1)
	finish := func(oc outcome) {
		select {
		case end <- oc:
		default:
		}
	}
	planned := make(chan struct{}, 1)

	// Reader: render and track.
	go func() {
		for {
			typ, data, err := conn.Read(cctx)
			if err != nil {
				finish(outcome{err: err})
				return
			}
			if typ == websocket.MessageBinary {
				_, _ = o.Out.Write(data)
				continue
			}
			var f ptyhost.ServerFrame
			if json.Unmarshal(data, &f) != nil {
				continue
			}
			switch f.Type {
			case "plan":
				if f.Plan == nil {
					continue
				}
				if f.Plan.Presentation != ptyevent.PresentationDirect {
					finish(outcome{err: ErrNotDirect, done: true})
					return
				}
				st.mu.Lock()
				st.plan, st.control, st.gen = *f.Plan, f.Plan.Controller, f.Plan.Generation
				// Everything the daemon already applied is not resent.
				keep := st.pending[:0]
				for _, p := range st.pending {
					if p.id > f.Plan.LastInputID {
						keep = append(keep, p)
					}
				}
				st.pending = keep
				if f.Plan.LastInputID >= st.nextID {
					st.nextID = f.Plan.LastInputID + 1
				}
				st.mu.Unlock()
				if mode == ptyhost.ModeInteractive && !f.Plan.Controller {
					fmt.Fprintf(o.Status, "\r\n[warden] viewing — %s controls this session; Ctrl-] t takes over, Ctrl-] . detaches\r\n", f.Plan.Holder)
				}
				select {
				case planned <- struct{}{}:
				default:
				}
			case "event":
				if f.Event == nil {
					continue
				}
				if r, ended := render(o, st, *f.Event); ended {
					finish(outcome{res: r, done: true})
					return
				}
			case "controller":
				st.mu.Lock()
				st.control, st.gen = true, f.Generation
				st.mu.Unlock()
				fmt.Fprint(o.Status, "\r\n[warden] you have control\r\n")
			case "demoted":
				st.mu.Lock()
				st.control = false
				st.mu.Unlock()
				fmt.Fprintf(o.Status, "\r\n[warden] %s took control; Ctrl-] t takes it back\r\n", f.Holder)
			case "rejected":
				st.mu.Lock()
				st.control = false
				st.mu.Unlock()
				fmt.Fprint(o.Status, "\r\n[warden] input refused (stale control); Ctrl-] t takes control\r\n")
			case "resync":
				finish(outcome{err: errors.New("stream fell behind: " + f.Reason)})
				return
			case "reject":
				st.mu.Lock()
				st.cursor = ptyevent.Cursor{} // discard state, attach fresh
				st.mu.Unlock()
				finish(outcome{err: errors.New("daemon rejected the cursor: " + f.Reason)})
				return
			}
		}
	}()

	// Writer: pending input (fenced), resize, takeover — only after the plan.
	go func() {
		select {
		case <-planned:
		case <-cctx.Done():
			return
		}
		sent := uint64(0)
		for {
			st.mu.Lock()
			var batch []pendingInput
			for _, p := range st.pending {
				if p.id > sent {
					batch = append(batch, p)
				}
			}
			gen, control := st.gen, st.control
			st.mu.Unlock()
			for _, p := range batch {
				if !control {
					// A viewer's keystrokes go nowhere; never queue them for a later
					// takeover (they would run out of context).
					sent = p.id
					st.dropThrough(p.id)
					continue
				}
				if err := send(ptyhost.ClientFrame{Type: "input", Gen: gen, ID: p.id, Data: p.data}); err != nil {
					finish(outcome{err: err})
					return
				}
				sent = p.id
			}
			select {
			case <-st.wake:
			case <-winch:
				if o.In != nil {
					if c, r, e := Size(o.In); e == nil {
						if send(ptyhost.ClientFrame{Type: "resize", Cols: c, Rows: r}) != nil {
							return
						}
					}
				}
			case <-takeover:
				_ = send(ptyhost.ClientFrame{Type: "takeover"})
			case <-cctx.Done():
				return
			}
		}
	}()

	select {
	case oc := <-end:
		return oc.res, oc.done, oc.err
	case <-detach:
		return Result{Reason: "detached"}, true, nil
	case <-ctx.Done():
		return Result{Reason: "detached"}, true, nil
	}
}

// render writes one journal event for the user and reports when the process
// ended. Output with seq <= head is replay (redacted, from the journal); later
// output arrives as raw frames, so those events only advance the cursor.
func render(o Options, st *state, ev ptyevent.Event) (Result, bool) {
	st.mu.Lock()
	plan := st.plan
	st.cursor = ptyevent.Cursor{SessionID: ev.SessionID, Epoch: ev.Epoch, Seq: ev.Seq}
	st.mu.Unlock()
	replay := ev.Seq <= plan.Head || !plan.Interactive
	switch ev.Kind {
	case ptyevent.KindOutput:
		if replay && ev.Output != nil {
			_, _ = o.Out.Write(ev.Output.Data)
		}
	case ptyevent.KindMarker:
		if ev.Marker == nil {
			break
		}
		switch ev.Marker.Type {
		case ptyevent.MarkerSnapshot:
			if replay {
				_, _ = o.Out.Write(ptyruntime.ScreenBytes([]byte(ev.Marker.Snapshot)))
			}
		case ptyevent.MarkerReset:
			if replay {
				_, _ = o.Out.Write([]byte("\x1b[H\x1b[2J"))
			}
		}
	case ptyevent.KindLifecycle:
		lc := ev.Lifecycle
		if lc != nil && lc.Phase.Terminal() && ev.Epoch == plan.Epoch {
			r := Result{Reason: "exited", ExitCode: lc.ExitCode, Signal: lc.Signal}
			msg := "process ended"
			switch {
			case lc.ExitCode != nil:
				msg = fmt.Sprintf("process exited with status %d", *lc.ExitCode)
			case lc.Signal != "":
				msg = "process killed by " + lc.Signal
			case lc.Detail != "":
				msg = lc.Detail
			}
			fmt.Fprintf(o.Status, "\r\n[warden] %s\r\n", msg)
			return r, true
		}
	}
	return Result{}, false
}
