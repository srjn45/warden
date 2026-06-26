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
