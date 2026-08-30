package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/store"
)

func TestWaitReturnsExistingMessage(t *testing.T) {
	srv, _, mb := newMsgServer(t)
	mb.Append(mailbox.Message{To: "agent-1", From: "agent-2", Body: "ready"})
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/sessions/agent-1/messages/wait?timeout=2")
	if err != nil {
		t.Fatalf("GET wait: %v", err)
	}
	defer resp.Body.Close()
	var wr struct {
		Found   bool             `json:"found"`
		Message *mailbox.Message `json:"message"`
	}
	json.NewDecoder(resp.Body).Decode(&wr)
	if !wr.Found || wr.Message == nil || wr.Message.Body != "ready" {
		t.Fatalf("got %+v", wr)
	}
	// the wait handler marked it read, so nothing unread remains
	if _, ok, _ := mb.TakeFirstUnread("agent-1", ""); ok {
		t.Fatalf("message should have been marked read by wait")
	}
}

func TestWaitTimesOut(t *testing.T) {
	srv, _, _ := newMsgServer(t)
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	start := time.Now()
	resp, err := http.Get(ts.URL + "/api/v1/sessions/empty/messages/wait?timeout=1")
	if err != nil {
		t.Fatalf("GET wait: %v", err)
	}
	defer resp.Body.Close()
	var wr struct {
		Found bool `json:"found"`
	}
	json.NewDecoder(resp.Body).Decode(&wr)
	if wr.Found {
		t.Fatalf("expected timeout (found=false)")
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatalf("returned too early — did not actually wait")
	}
}

func TestWaitWakesOnDeliveredMessage(t *testing.T) {
	srv, fs, _ := newMsgServer(t)
	fs.Insert(context.Background(), &store.Session{ID: "agent-1", TmuxSession: "agent-1", Status: store.StatusWorking})
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	done := make(chan string, 1)
	go func() {
		resp, _ := http.Get(ts.URL + "/api/v1/sessions/agent-1/messages/wait?timeout=5")
		var wr struct {
			Found   bool             `json:"found"`
			Message *mailbox.Message `json:"message"`
		}
		json.NewDecoder(resp.Body).Decode(&wr)
		resp.Body.Close()
		if wr.Message != nil {
			done <- wr.Message.Body
		} else {
			done <- ""
		}
	}()

	time.Sleep(150 * time.Millisecond) // let the waiter subscribe + do its first check
	// deliver via the real send endpoint (calls notify() → hub → waiter re-checks)
	http.Post(ts.URL+"/api/v1/sessions/agent-1/messages", "application/json", strings.NewReader(`{"from":"agent-2","body":"ping"}`))

	select {
	case got := <-done:
		if got != "ping" {
			t.Fatalf("want ping, got %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wait did not return after a message was delivered")
	}
}

// TestWaitHonorsFromFilter is the orchestrator-awaits-a-specific-worker path:
// an orchestrator blocks on wait?from=worker-2. A message from a DIFFERENT
// worker must NOT wake it (it stays blocked, and that message is left unread);
// only the awaited sender's message wakes it and is returned.
func TestWaitHonorsFromFilter(t *testing.T) {
	srv, fs, mb := newMsgServer(t)
	fs.Insert(context.Background(), &store.Session{ID: "orch", TmuxSession: "orch", Status: store.StatusWorking})
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	done := make(chan *mailbox.Message, 1)
	go func() {
		resp, _ := http.Get(ts.URL + "/api/v1/sessions/orch/messages/wait?timeout=5&from=worker-2")
		var wr struct {
			Found   bool             `json:"found"`
			Message *mailbox.Message `json:"message"`
		}
		json.NewDecoder(resp.Body).Decode(&wr)
		resp.Body.Close()
		done <- wr.Message
	}()

	time.Sleep(150 * time.Millisecond) // let the waiter subscribe + do its first check
	// A message from the WRONG sender must not wake the from-filtered wait.
	http.Post(ts.URL+"/api/v1/sessions/orch/messages", "application/json",
		strings.NewReader(`{"from":"worker-3","body":"noise"}`))

	select {
	case m := <-done:
		t.Fatalf("wait returned on a non-matching sender: %+v", m)
	case <-time.After(400 * time.Millisecond):
		// good — still blocked despite the worker-3 message
	}

	// Now the awaited sender delivers — this must wake the wait.
	http.Post(ts.URL+"/api/v1/sessions/orch/messages", "application/json",
		strings.NewReader(`{"from":"worker-2","body":"done"}`))

	select {
	case m := <-done:
		if m == nil || m.From != "worker-2" || m.Body != "done" {
			t.Fatalf("want worker-2's message, got %+v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wait did not return after the awaited sender delivered")
	}

	// The non-matching worker-3 message was skipped, not consumed — it is still
	// unread and readable by an unfiltered take.
	m, ok, err := mb.TakeFirstUnread("orch", "")
	if err != nil || !ok || m.From != "worker-3" {
		t.Fatalf("worker-3 message should remain unread; got ok=%v from=%q err=%v", ok, m.From, err)
	}
}

// TestWaitFromFilterTimesOut proves the from-filtered wait honors its timeout
// when only non-matching senders arrive — the orchestrator wakes with
// found=false rather than blocking forever or returning the wrong message.
func TestWaitFromFilterTimesOut(t *testing.T) {
	srv, fs, _ := newMsgServer(t)
	fs.Insert(context.Background(), &store.Session{ID: "orch", TmuxSession: "orch", Status: store.StatusWorking})
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	go func() {
		time.Sleep(150 * time.Millisecond)
		http.Post(ts.URL+"/api/v1/sessions/orch/messages", "application/json",
			strings.NewReader(`{"from":"someone-else","body":"noise"}`))
	}()

	start := time.Now()
	resp, err := http.Get(ts.URL + "/api/v1/sessions/orch/messages/wait?timeout=1&from=worker-2")
	if err != nil {
		t.Fatalf("GET wait: %v", err)
	}
	defer resp.Body.Close()
	var wr struct {
		Found bool `json:"found"`
	}
	json.NewDecoder(resp.Body).Decode(&wr)
	if wr.Found {
		t.Fatalf("expected timeout (found=false) — a non-matching sender must not satisfy the filter")
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatalf("returned too early — did not honor the timeout")
	}
}
