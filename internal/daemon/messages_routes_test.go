package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/srajanpathak/agentctl/internal/mailbox"
	"github.com/srajanpathak/agentctl/internal/store"
)

// newMsgServer builds a Server wired with a real temp mailbox, a fake store, and
// a fake lifecycle, plus a hub + done channel (used by the wait handler too).
func newMsgServer(t *testing.T) (*Server, *fakeStore, *mailbox.Store) {
	t.Helper()
	mb, err := mailbox.New(t.TempDir())
	if err != nil {
		t.Fatalf("mailbox.New: %v", err)
	}
	fs := newFakeStore()
	srv := &Server{store: fs, life: &fakeLife{}, mbox: mb, hub: newHub(), done: make(chan struct{})}
	return srv, fs, mb
}

func TestSendMessageStoresAndWakesParked(t *testing.T) {
	srv, fs, _ := newMsgServer(t)
	fs.Insert(context.Background(), &store.Session{ID: "agent-1", TmuxSession: "agent-1", Status: store.StatusIdle})
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	body := strings.NewReader(`{"from":"agent-2","body":"need a hand"}`)
	resp, err := http.Post(ts.URL+"/sessions/agent-1/messages", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var sr struct {
		Message mailbox.Message `json:"message"`
		Woke    bool            `json:"woke"`
	}
	json.NewDecoder(resp.Body).Decode(&sr)
	if sr.Message.Body != "need a hand" || sr.Message.From != "agent-2" {
		t.Fatalf("message wrong: %+v", sr.Message)
	}
	if !sr.Woke {
		t.Fatalf("idle recipient should have been woken")
	}
	if fl := srv.life.(*fakeLife); !strings.Contains(fl.lastInput, "New message from agent-2") {
		t.Fatalf("wake notice not injected: %q", fl.lastInput)
	}
}

func TestSendMessageWorkingRecipientNotWoken(t *testing.T) {
	srv, fs, _ := newMsgServer(t)
	fs.Insert(context.Background(), &store.Session{ID: "busy", TmuxSession: "busy", Status: store.StatusWorking})
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/sessions/busy/messages", "application/json", strings.NewReader(`{"from":"x","body":"hi"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var sr struct {
		Woke bool `json:"woke"`
	}
	json.NewDecoder(resp.Body).Decode(&sr)
	if sr.Woke {
		t.Fatalf("working recipient must NOT be woken")
	}
	if fl := srv.life.(*fakeLife); fl.lastInput != "" {
		t.Fatalf("no input should have been injected, got %q", fl.lastInput)
	}
}

func TestSendMessageUnknownRecipient404(t *testing.T) {
	srv, _, _ := newMsgServer(t)
	ts := httptest.NewServer(srv.router())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/sessions/ghost/messages", "application/json", strings.NewReader(`{"body":"hi"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

func TestSendMessageEmptyBody400(t *testing.T) {
	srv, fs, _ := newMsgServer(t)
	fs.Insert(context.Background(), &store.Session{ID: "agent-1", TmuxSession: "agent-1", Status: store.StatusIdle})
	ts := httptest.NewServer(srv.router())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/sessions/agent-1/messages", "application/json", bytes.NewBufferString(`{"from":"x","body":""}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestInboxListsAndMarksRead(t *testing.T) {
	srv, fs, mb := newMsgServer(t)
	fs.Insert(context.Background(), &store.Session{ID: "agent-1", TmuxSession: "agent-1", Status: store.StatusIdle})
	mb.Append(mailbox.Message{To: "agent-1", From: "x", Body: "one"})
	mb.Append(mailbox.Message{To: "agent-1", From: "y", Body: "two"})
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	resp, _ := http.Get(ts.URL + "/sessions/agent-1/messages")
	var ir struct {
		Messages []mailbox.Message `json:"messages"`
	}
	json.NewDecoder(resp.Body).Decode(&ir)
	resp.Body.Close()
	if len(ir.Messages) != 2 {
		t.Fatalf("want 2 messages, got %d", len(ir.Messages))
	}

	// after reading, ?unread=true returns none
	resp, _ = http.Get(ts.URL + "/sessions/agent-1/messages?unread=true")
	var ir2 struct {
		Messages []mailbox.Message `json:"messages"`
	}
	json.NewDecoder(resp.Body).Decode(&ir2)
	resp.Body.Close()
	if len(ir2.Messages) != 0 {
		t.Fatalf("want 0 unread after read, got %d", len(ir2.Messages))
	}
}
