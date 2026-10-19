package ptyhost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/srjn45/warden/internal/ptyevent"
)

// wsServer serves the §6 protocol for one session name of whichever Manager
// cur() returns (so a test can swap in a "restarted daemon").
func wsServer(t *testing.T, cur func() *Manager, name string, observeOnly bool) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := cur()
		tg, ok := m.Lookup(name)
		if !ok {
			http.Error(w, "no journal", http.StatusConflict)
			return
		}
		if observeOnly {
			tg.Direct = nil
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_ = m.Serve(r.Context(), conn, tg, "sock-"+r.URL.Query().Get("c"))
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

type wsClient struct {
	t      *testing.T
	conn   *websocket.Conn
	frames chan frame
	mu     sync.Mutex
	raw    strings.Builder
	events []ptyevent.Event
	plans  []PlanFrame
	kinds  []string
	srv    []ServerFrame // every text frame, for tests that need frame fields
	cancel context.CancelFunc
	done   chan struct{}
}

type frame struct {
	text   *ServerFrame
	binary []byte
}

func dial(t *testing.T, url string) *wsClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	conn.SetReadLimit(4 << 20)
	c := &wsClient{t: t, conn: conn, frames: make(chan frame, 4096), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(c.done)
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			c.mu.Lock()
			if typ == websocket.MessageBinary {
				c.raw.Write(data)
			} else {
				var f ServerFrame
				if json.Unmarshal(data, &f) == nil {
					c.kinds = append(c.kinds, f.Type)
					c.srv = append(c.srv, f)
					switch {
					case f.Type == "event" && f.Event != nil:
						c.events = append(c.events, *f.Event)
					case f.Type == "plan" && f.Plan != nil:
						c.plans = append(c.plans, *f.Plan)
					}
				}
			}
			c.mu.Unlock()
		}
	}()
	t.Cleanup(c.close)
	return c
}

func (c *wsClient) close() {
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
	c.cancel()
	<-c.done
}

func (c *wsClient) send(v any) {
	c.t.Helper()
	b, _ := json.Marshal(v)
	if err := c.conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *wsClient) keys(s string) {
	c.t.Helper()
	if err := c.conn.Write(context.Background(), websocket.MessageBinary, []byte(s)); err != nil {
		c.t.Fatal(err)
	}
}

func (c *wsClient) snapshot() (raw string, evs []ptyevent.Event, plans []PlanFrame, kinds []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.raw.String(), append([]ptyevent.Event(nil), c.events...), append([]PlanFrame(nil), c.plans...), append([]string(nil), c.kinds...)
}

func (c *wsClient) waitEventOutput(want string) {
	c.t.Helper()
	eventually(c.t, "event output "+want, func() bool { _, evs, _, _ := c.snapshot(); return strings.Contains(outputOf(evs), want) })
}

func (c *wsClient) waitRaw(want string) {
	c.t.Helper()
	eventually(c.t, "raw output "+want, func() bool { raw, _, _, _ := c.snapshot(); return strings.Contains(raw, want) })
}

func (c *wsClient) waitPlans(n int) []PlanFrame {
	c.t.Helper()
	var plans []PlanFrame
	eventually(c.t, "plan frame", func() bool { _, _, plans, _ = c.snapshot(); return len(plans) >= n })
	return plans
}

func (c *wsClient) cursor() ptyevent.Cursor {
	_, evs, _, _ := c.snapshot()
	last := evs[len(evs)-1]
	return ptyevent.Cursor{SessionID: last.SessionID, Epoch: last.Epoch, Seq: last.Seq}
}

func startDirect(t *testing.T, m *Manager, name string) {
	t.Helper()
	if err := m.TerminalHost().NewSession(context.Background(), name, t.TempDir(), fakeEnv+"=repl"); err != nil {
		t.Fatal(err)
	}
}

func TestTransportInteractiveInputResizeAndReplay(t *testing.T) {
	noLeaks(t)
	run := newTmux(t)
	dir := filepath.Join(t.TempDir(), "pty")
	m := newManager(t, run, dir, ModeOn)
	defer m.Close()
	startDirect(t, m, "term-1")
	url := wsServer(t, func() *Manager { return m }, "term-1", false)

	c := dial(t, url+"?c=a")
	c.send(ClientFrame{Type: "hello", Mode: ModeInteractive, Cols: 100, Rows: 30})
	plan := c.waitPlans(1)[0]
	if !plan.Interactive || plan.Presentation != ptyevent.PresentationDirect || plan.ClientID != "sock-a" {
		t.Fatalf("plan = %+v", plan)
	}
	c.waitEventOutput("ready") // fresh attach replays from the start
	c.keys("echo over-ws\r")
	c.waitRaw("said:over-ws")         // live, unredacted bytes
	c.waitEventOutput("said:over-ws") // and the journaled event
	c.send(ClientFrame{Type: "resize", Cols: 120, Rows: 40})
	c.keys("size\r")
	c.waitRaw("size:120x40")

	_, evs, _, _ := c.snapshot()
	var humanIn, resized bool
	for _, ev := range evs {
		humanIn = humanIn || (ev.Kind == ptyevent.KindInput && ev.Input.Origin == ptyevent.OriginHuman && ev.Input.ClientID == "sock-a")
		resized = resized || (ev.Kind == ptyevent.KindResize && ev.Resize.Cols == 120 && ev.Resize.Rows == 40 && ev.Resize.Origin == ptyevent.OriginHuman)
	}
	if !humanIn || !resized {
		t.Fatalf("journal must record human input (%v) and the resize (%v) with this socket as client", humanIn, resized)
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].Seq != evs[i-1].Seq+1 {
			t.Fatalf("stream not contiguous at %d: %d after %d", i, evs[i].Seq, evs[i-1].Seq)
		}
	}
	cur := c.cursor()
	c.send(ClientFrame{Type: "ack", Seq: cur.Seq})
	c.close()

	// Reconnect with the cursor: only what happened since, no duplicates.
	_ = m.Host().SendKeys(context.Background(), "term-1", "echo while-away", "Enter")
	c2 := dial(t, url+"?c=b")
	c2.send(ClientFrame{Type: "hello", Mode: ModeObserve, Cursor: &cur})
	if p := c2.waitPlans(1)[0]; p.Mode != ptyevent.ResumeReplay || p.Interactive {
		t.Fatalf("reconnect plan = %+v, want observe replay", p)
	}
	c2.waitEventOutput("said:while-away")
	_, evs2, _, _ := c2.snapshot()
	if evs2[0].Seq != cur.Seq+1 {
		t.Fatalf("resume started at seq %d, want %d", evs2[0].Seq, cur.Seq+1)
	}
	if raw, _, _, _ := c2.snapshot(); raw != "" {
		t.Fatal("observe client got raw bytes")
	}
	c2.keys("echo ignored\r") // observe mode: input is dropped
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(liveOutput(t, m, "term-1"), "said:ignored") {
		t.Fatal("observe client input reached the PTY")
	}
	c2.close()

	// A hello without a cursor from the acking client resumes from its ack.
	c3 := dial(t, url+"?c=x")
	c3.send(ClientFrame{Type: "hello", Mode: ModeObserve, ClientID: "sock-a"})
	if p := c3.waitPlans(1)[0]; p.Mode != ptyevent.ResumeReplay || p.FromSeq != cur.Seq+1 {
		t.Fatalf("ack resume plan = %+v, want replay from %d", p, cur.Seq+1)
	}
}

// Redaction boundary: an interactive client sees its own live bytes, but the
// journal and every replay carry only the placeholder.
func TestTransportReplayIsRedacted(t *testing.T) {
	noLeaks(t)
	run := newTmux(t)
	dir := filepath.Join(t.TempDir(), "pty")
	m := newManager(t, run, dir, ModeOn)
	defer m.Close()
	startDirect(t, m, "term-1")
	url := wsServer(t, func() *Manager { return m }, "term-1", false)
	secret := fakeSecret()

	live := dial(t, url)
	live.send(ClientFrame{Type: "hello", Mode: ModeInteractive})
	live.waitPlans(1)
	live.waitEventOutput("ready")
	live.keys("secret\r")
	live.waitRaw("token=" + secret)
	live.waitEventOutput("token=[REDACTED")
	_, liveEvs, _, _ := live.snapshot()
	if strings.Contains(outputOf(liveEvs), secret) {
		t.Fatal("live event frames carried the unredacted secret")
	}
	live.close()
	// The server journals the detach and then its snapshot asynchronously after
	// the socket closes; re-attach only once the snapshot exists, or a loaded
	// machine can win the race and see "no snapshot".
	eventually(t, "detach snapshot", func() bool {
		tg, ok := m.Lookup("term-1")
		if !ok {
			return false
		}
		var snap bool
		_ = tg.Stream.Journal().Read(1, func(ev ptyevent.Event) error {
			if ev.Kind == ptyevent.KindMarker && ev.Marker != nil && ev.Marker.Type == ptyevent.MarkerSnapshot {
				snap = true
			}
			return nil
		})
		return snap
	})

	replay := dial(t, url)
	replay.send(ClientFrame{Type: "hello", Mode: ModeInteractive})
	// The clean detach above took a snapshot, so a fresh attach resumes from
	// it: the token is inside the (redacted) snapshot text.
	if p := replay.waitPlans(1)[0]; p.Mode != ptyevent.ResumeSnapshot {
		t.Fatalf("fresh attach after a clean detach = %+v, want snapshot", p)
	}
	eventually(t, "redacted replay", func() bool {
		_, evs, _, _ := replay.snapshot()
		return strings.Contains(screenText(evs), "token=[REDACTED")
	})
	raw, evs, _, _ := replay.snapshot()
	if strings.Contains(screenText(evs), secret) || strings.Contains(raw, secret) {
		t.Fatal("replay to a reconnecting client carried the unredacted secret")
	}
	if strings.Contains(liveOutput(t, m, "term-1"), secret) {
		t.Fatal("unredacted secret reached the journal")
	}
}

// A daemon restart kills a direct shell (the daemon owns its PTY). The next
// daemon recovers the terminal onto the same journal under a new epoch, and a
// client reconnecting with its pre-restart cursor is resynchronized from the
// reset snapshot rather than replayed into a screen it no longer matches.
func TestTransportReconnectAcrossDaemonRestart(t *testing.T) {
	noLeaks(t)
	run := newTmux(t)
	dir := filepath.Join(t.TempDir(), "pty")
	var mu sync.Mutex
	m := newManager(t, run, dir, ModeOn)
	cur := func() *Manager { mu.Lock(); defer mu.Unlock(); return m }
	startDirect(t, m, "term-1")
	url := wsServer(t, cur, "term-1", false)

	c := dial(t, url)
	c.send(ClientFrame{Type: "hello", Mode: ModeInteractive})
	c.waitPlans(1)
	c.keys("echo epoch-one\r")
	c.waitEventOutput("said:epoch-one")
	before := c.cursor()
	m.Close() // daemon stops: the shell dies with it and the socket ends
	eventually(t, "socket closed by restart", func() bool {
		select {
		case <-c.done:
			return true
		default:
			return false
		}
	})

	m2 := newManager(t, run, dir, ModeOn)
	defer m2.Close()
	mu.Lock()
	m = m2
	mu.Unlock()
	if !m2.HasDirectJournal("term-1") {
		t.Fatal("restart lost the direct journal")
	}
	startDirect(t, m2, "term-1") // the daemon's startup recovery respawns it

	c2 := dial(t, url)
	c2.send(ClientFrame{Type: "hello", Mode: ModeInteractive, Cursor: &before})
	plan := c2.waitPlans(1)[0]
	if plan.Mode != ptyevent.ResumeSnapshot || plan.Epoch != before.Epoch+1 {
		t.Fatalf("plan after restart = %+v, want snapshot on epoch %d", plan, before.Epoch+1)
	}
	eventually(t, "snapshot event", func() bool { _, evs, _, _ := c2.snapshot(); return len(evs) > 0 })
	_, evs, _, _ := c2.snapshot()
	if evs[0].Kind != ptyevent.KindMarker || evs[0].Marker.Type != ptyevent.MarkerSnapshot {
		t.Fatalf("first event after restart = %+v, want the snapshot to reset to", evs[0])
	}
	c2.keys("echo epoch-two\r")
	c2.waitRaw("said:epoch-two")

	all := liveEvents(t, m2, "term-1")
	validStream(t, all)
	var phases []string
	for _, ev := range all {
		if ev.Kind == ptyevent.KindLifecycle && ev.Lifecycle.Phase != ptyevent.PhaseAttached && ev.Lifecycle.Phase != ptyevent.PhaseDetached {
			phases = append(phases, string(ev.Lifecycle.Phase))
		}
	}
	if got := strings.Join(phases, ","); !strings.HasPrefix(got, "spawned,") || !strings.Contains(got, ",spawned") {
		t.Fatalf("lifecycle phases %q: want epoch 1 spawned → exit → epoch 2 spawned", got)
	}
}

func TestTransportShadowIsObserveOnly(t *testing.T) {
	noLeaks(t)
	run := newTmux(t)
	dir := filepath.Join(t.TempDir(), "pty")
	m := newManager(t, run, dir, ModeShadow)
	defer m.Close()
	ctx := context.Background()
	if err := m.Host().NewSession(ctx, "agent-1", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	url := wsServer(t, func() *Manager { return m }, "agent-1", false)
	c := dial(t, url)
	c.send(ClientFrame{Type: "hello", Mode: ModeInteractive})
	if p := c.waitPlans(1)[0]; p.Interactive || p.Presentation != ptyevent.PresentationTmux {
		t.Fatalf("shadow plan = %+v, want observe-only tmux", p)
	}
	_ = m.Host().SendKeys(ctx, "agent-1", "printf 'mirror-%s\\n' live", "Enter")
	c.waitEventOutput("mirror-live")
	if raw, _, _, _ := c.snapshot(); raw != "" {
		t.Fatal("shadow stream sent raw bytes")
	}
}

func TestTransportRequiresHello(t *testing.T) {
	noLeaks(t)
	run := newTmux(t)
	m := newManager(t, run, filepath.Join(t.TempDir(), "pty"), ModeOn)
	defer m.Close()
	startDirect(t, m, "term-1")
	c := dial(t, wsServer(t, func() *Manager { return m }, "term-1", false))
	c.send(ClientFrame{Type: "ack", Seq: 1})
	eventually(t, "reject", func() bool { _, _, _, k := c.snapshot(); return len(k) > 0 && k[0] == "reject" })
}

// screenText is everything a replaying client renders: snapshot text and
// output data.
func screenText(evs []ptyevent.Event) string {
	var b strings.Builder
	for _, ev := range evs {
		switch {
		case ev.Kind == ptyevent.KindMarker && ev.Marker.Type == ptyevent.MarkerSnapshot:
			b.WriteString(ev.Marker.Snapshot)
		case ev.Kind == ptyevent.KindOutput:
			b.Write(ev.Output.Data)
		}
	}
	return b.String()
}
