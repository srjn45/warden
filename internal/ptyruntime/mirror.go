package ptyruntime

import (
	"errors"
	"io"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/ptyevent"
)

// MirrorConfig describes a shadow Mirror (M1 `runtime.pty: shadow`): the
// journal of a process warden does NOT host — a tmux pane whose output is
// copied to Source by `tmux pipe-pane -O`.
type MirrorConfig struct {
	// Journal receives the events (caller-owned, like Config.Journal).
	Journal *Journal
	// Source yields the pane's output bytes. The Mirror reads it until EOF or
	// an error; EOF means the pipe closed (the pane died or the pipe was
	// stopped).
	Source io.Reader
	// Recover marks a re-adoption of a process that outlived the previous
	// mirror (daemon restart): the epoch opens with lifecycle{recovered}
	// instead of lifecycle{spawned}, and an unended previous epoch is not
	// journaled as crashed (the process is alive).
	Recover bool
	// Seed is the pane's visible text at adoption. It is rendered into the
	// screen model so the reset snapshot reflects the live screen; it is
	// redacted before use.
	Seed       string
	Cols, Rows uint16
	Backend    string
	Options    Options
}

// Mirror journals a tmux-hosted pane's output through the same writer
// pipeline as a Session (redaction, control scanning, screen model, snapshots,
// fan-out). It is observe-only: warden writes nothing to the pane through it.
type Mirror struct {
	writer
	src   io.Reader
	queue chan []byte

	ended  bool // guarded by writer.mu
	exitEv *ptyevent.Lifecycle

	readDone   chan struct{} // Source hit EOF/error
	writerDone chan struct{}
	closeOnce  sync.Once
}

// StartMirror opens a new epoch on cfg.Journal for the mirrored pane and
// starts copying Source into it. A non-empty journal gets marker{reset} and a
// snapshot after the opening lifecycle event, since bytes the pane produced
// while nobody mirrored it are gone (§6.6).
func StartMirror(cfg MirrorConfig) (*Mirror, error) {
	if cfg.Journal == nil {
		return nil, errors.New("ptyruntime: MirrorConfig.Journal is required")
	}
	if cfg.Source == nil {
		return nil, errors.New("ptyruntime: MirrorConfig.Source is required")
	}
	if cfg.Cols == 0 {
		cfg.Cols = 80
	}
	if cfg.Rows == 0 {
		cfg.Rows = 24
	}
	opt := cfg.Options.withDefaults()
	m := &Mirror{writer: newWriter(opt, cfg.Journal, cfg.Cols, cfg.Rows), src: cfg.Source,
		queue: make(chan []byte, opt.QueueDepth), readDone: make(chan struct{}), writerDone: make(chan struct{})}
	m.mu.Lock()
	err := m.open(cfg)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	go m.readLoop()
	go m.writeLoop()
	return m, nil
}

func (m *Mirror) open(cfg MirrorConfig) error {
	last := m.j.Last()
	if last.Seq != 0 && !m.j.Ended() && !cfg.Recover {
		ev := m.event(ptyevent.KindLifecycle)
		ev.Epoch, ev.MonoNS = last.Epoch, last.MonoNS
		ev.Lifecycle = &ptyevent.Lifecycle{Phase: ptyevent.PhaseCrashed, Backend: cfg.Backend,
			Presentation: ptyevent.PresentationTmux, Detail: "mirror lost the pane before its exit was journaled"}
		if err := m.commit(ev); err != nil {
			return err
		}
	}
	phase := ptyevent.PhaseSpawned
	if cfg.Recover {
		phase = ptyevent.PhaseRecovered
	}
	ev := m.event(ptyevent.KindLifecycle)
	ev.Lifecycle = &ptyevent.Lifecycle{Phase: phase, Backend: cfg.Backend, Presentation: ptyevent.PresentationTmux}
	if err := m.commit(ev); err != nil {
		return err
	}
	if cfg.Seed != "" {
		seed, _ := ptyevent.Redact([]byte(cfg.Seed))
		_, _ = m.screen.Write(ScreenBytes(seed))
	}
	if last.Seq != 0 {
		ev := m.event(ptyevent.KindMarker)
		ev.Marker = &ptyevent.Marker{Type: ptyevent.MarkerReset}
		if err := m.commit(ev); err != nil {
			return err
		}
		return m.snapshot()
	}
	if cfg.Seed != "" {
		return m.snapshot()
	}
	return nil
}

// ScreenBytes turns screen text (LF-separated lines, as Screen and
// capture-pane render it) into terminal output that redraws it from the home
// position.
func ScreenBytes(text []byte) []byte {
	out := make([]byte, 0, len(text)+16)
	out = append(out, "\x1b[H\x1b[2J"...)
	for _, b := range text {
		if b == '\n' {
			out = append(out, '\r')
		}
		out = append(out, b)
	}
	return out
}

// Done is closed once Source has reached EOF (or failed) and everything it
// delivered has been journaled.
func (m *Mirror) Done() <-chan struct{} { return m.writerDone }

// Exit journals the end of the mirrored process (phase exited, signaled or
// crashed). Output arriving afterwards is dropped (no-io-after-exit). It is a
// no-op after the first call.
func (m *Mirror) Exit(lc ptyevent.Lifecycle) error {
	if !lc.Phase.Terminal() {
		return ptyevent.ErrPhase
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ended || m.closed {
		return nil
	}
	m.flushOutput()
	lc.Presentation = ptyevent.PresentationTmux
	ev := m.event(ptyevent.KindLifecycle)
	ev.Lifecycle = &lc
	err := m.commit(ev)
	m.ended, m.exitEv = true, &lc
	return err
}

// Exited returns the journaled exit (nil while the pane is live).
func (m *Mirror) Exited() *ptyevent.Lifecycle {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.exitEv
}

// Close stops mirroring: it closes Source when it is an io.Closer, waits for
// the reader and writer to drain (the caller must make sure Source reaches
// EOF — e.g. by stopping the pipe-pane), then ends every subscription. The
// journal stays open and the epoch stays open unless Exit was called: a live
// pane is re-adopted later with Recover.
func (m *Mirror) Close() error {
	m.closeOnce.Do(func() {
		if c, ok := m.src.(io.Closer); ok {
			_ = c.Close()
		}
		<-m.writerDone
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		close(m.closing)
		m.hub.close()
		m.wg.Wait()
	})
	return nil
}

func (m *Mirror) readLoop() {
	defer close(m.readDone)
	for {
		buf := make([]byte, m.opt.ReadBuffer)
		n, err := m.src.Read(buf)
		if n > 0 {
			m.queue <- buf[:n] // backpressure goes to tmux, which buffers the pipe
		}
		if err != nil {
			return
		}
	}
}

func (m *Mirror) writeLoop() {
	defer close(m.writerDone)
	idle := time.NewTimer(time.Hour)
	idle.Stop()
	defer idle.Stop()
	handle := func(data []byte) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.ended {
			return
		}
		if out, reds, ok := m.in.feed(data); ok {
			m.commitOutput(out, reds)
		}
		if m.in.pending() {
			idle.Reset(m.opt.IdleFlush)
		}
	}
	for {
		select {
		case data := <-m.queue:
			handle(data)
		case <-idle.C:
			m.mu.Lock()
			if !m.ended {
				m.flushOutput()
			}
			m.mu.Unlock()
		case <-m.readDone:
			for drained := false; !drained; {
				select {
				case data := <-m.queue:
					handle(data)
				default:
					drained = true
				}
			}
			m.mu.Lock()
			if !m.ended {
				m.flushOutput()
			}
			m.mu.Unlock()
			return
		}
	}
}
