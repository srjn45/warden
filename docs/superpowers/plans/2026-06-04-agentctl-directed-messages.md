# Directed Messages (Phase 2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add agent-to-agent directed messages — a durable per-recipient inbox, a "wake the recipient only if it's parked" delivery rule, and a cheap blocking `msg wait` (server long-poll) so an agent can await a reply in a single Bash call without burning LLM turns.

**Architecture:** A new pure `internal/mailbox` package persists each recipient's messages as one JSON file (`<data-dir>/inbox/<id>.json`), mutated atomically under a mutex (mirrors `internal/ctxstore` / `internal/store/file.go`). The daemon exposes `POST /sessions/{id}/messages` (send — appends + wakes a parked recipient via the existing `lifecycle.Input`), `GET /sessions/{id}/messages` (inbox — marks read), and a long-poll `GET /sessions/{id}/messages/wait` that subscribes to the existing `hub` and returns when a matching message arrives or the timeout fires. Client wraps all three; CLI adds `agentctl msg send/inbox/wait`; MCP mirrors `send_message`/`read_inbox` (NOT wait — a blocking MCP call would hit client timeouts; waiting is a CLI/Bash concern).

**Tech Stack:** Go, chi router, cobra CLI, MCP SDK. Module: `github.com/srajanpathak/agentctl`.

**Phase 3 note:** Agent self-identification env vars (`AGENTCTL_SESSION_ID`) are wired in Phase 3, not here. Until then, agents/humans pass `--as <id>` (CLI) or the `agent` field (MCP) explicitly; the primitives are fully functional without the env.

**Safety property:** A working recipient is NEVER interrupted — the daemon injects a wake notice only when the recipient's status is `idle` or `waiting_for_input`. Otherwise the message sits in the durable inbox until the recipient checks it.

---

## File Structure

- **Create** `internal/mailbox/mailbox.go` — `Message`, `Store` (per-recipient JSON file), `New`, `Append`, `Messages`, `MarkRead`, `TakeFirstUnread`, `ErrBadRecipient`. One responsibility: persist/retrieve per-recipient messages.
- **Create** `internal/mailbox/mailbox_test.go` — table tests.
- **Create** `internal/daemon/messages_routes.go` — `registerMessageRoutes`, `handleSendMessage`, `handleInbox`, `parked` helper, request/response types.
- **Create** `internal/daemon/messages_wait.go` — `handleWaitMessage` (blocking long-poll) + its response type + timeout consts.
- **Create** `internal/daemon/messages_routes_test.go` — httptest tests + the shared `newMsgServer` helper.
- **Create** `internal/daemon/messages_wait_test.go` — wait-handler tests (present / timeout / hub-wake).
- **Modify** `internal/daemon/api.go` — add `mbox *mailbox.Store` field to `Server`; register message routes in `router()`.
- **Modify** `internal/daemon/server.go` — add `*mailbox.Store` param to `NewServer`.
- **Modify** `internal/cli/daemon.go` — construct the mailbox store, pass it to `NewServer`.
- **Modify** `internal/daemon/server_test.go` — update the `NewServer(...)` call.
- **Modify** `internal/client/client.go` — `Message` type + `MsgSend`/`MsgInbox`/`MsgWait`.
- **Modify** `internal/client/client_test.go` — client round-trip tests.
- **Create** `internal/cli/messages.go` — the `msg` command group + pure identity/format helpers.
- **Create** `internal/cli/messages_test.go` — tests for the pure helpers.
- **Modify** `internal/cli/root.go` — register `newMsgCmd()`.
- **Modify** `internal/mcp/server.go` — `send_message`/`read_inbox` tools + arg structs.
- **Modify** `internal/mcp/server_test.go` — guard test.
- **Modify** `docs/USAGE.md` — document the `msg` commands.

---

## Task 1: mailbox package — Append & Messages

**Files:**
- Create: `internal/mailbox/mailbox.go`
- Test: `internal/mailbox/mailbox_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/mailbox/mailbox_test.go`:

```go
package mailbox

import (
	"errors"
	"testing"
)

func TestAppendAssignsIDAndTS(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, err := s.Append(Message{To: "agent-1", From: "agent-2", Body: "hello"})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if m.ID != "1" || m.Read || m.TS.IsZero() {
		t.Fatalf("got %+v", m)
	}
	m2, _ := s.Append(Message{To: "agent-1", From: "agent-2", Body: "again"})
	if m2.ID != "2" {
		t.Fatalf("want sequential id 2, got %q", m2.ID)
	}
}

func TestMessagesChronologicalPerRecipient(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Append(Message{To: "agent-1", From: "x", Body: "a"})
	s.Append(Message{To: "agent-1", From: "y", Body: "b"})
	s.Append(Message{To: "agent-2", From: "z", Body: "c"})

	one, err := s.Messages("agent-1")
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(one) != 2 || one[0].Body != "a" || one[1].Body != "b" {
		t.Fatalf("agent-1 inbox wrong: %+v", one)
	}
	two, _ := s.Messages("agent-2")
	if len(two) != 1 || two[0].Body != "c" {
		t.Fatalf("agent-2 inbox wrong: %+v", two)
	}
}

func TestMessagesEmptyReturnsEmptySlice(t *testing.T) {
	s, _ := New(t.TempDir())
	got, err := s.Messages("nobody")
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("want empty non-nil slice, got %#v", got)
	}
}

func TestAppendBadRecipientRejected(t *testing.T) {
	s, _ := New(t.TempDir())
	if _, err := s.Append(Message{To: "a/b", From: "x", Body: "v"}); !errors.Is(err, ErrBadRecipient) {
		t.Fatalf("want ErrBadRecipient, got %v", err)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s1, _ := New(dir)
	s1.Append(Message{To: "agent-1", From: "x", Body: "kept"})
	s2, err := New(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, _ := s2.Messages("agent-1")
	if len(got) != 1 || got[0].Body != "kept" {
		t.Fatalf("not persisted: %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mailbox/...`
Expected: FAIL — package has no buildable files / `undefined: New`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/mailbox/mailbox.go`:

```go
// Package mailbox is a daemon-owned per-recipient message store — the durable
// inbox behind agent-to-agent directed messages. Each recipient's messages live
// in one JSON file (<dir>/<id>.json), rewritten atomically (temp file + rename)
// under a mutex. Localhost session-store scale, like internal/ctxstore.
package mailbox

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/srajanpathak/agentctl/internal/store"
)

// ErrBadRecipient is returned when a recipient id is unsafe as a filename.
var ErrBadRecipient = errors.New("invalid recipient")

// Message is one directed message in a recipient's inbox.
type Message struct {
	ID   string    `json:"id"`   // per-inbox sequence, 1-based
	From string    `json:"from"` // sender id, or "human"/"daemon"
	To   string    `json:"to"`
	Body string    `json:"body"`
	TS   time.Time `json:"ts"`
	Read bool      `json:"read"`
}

// Store persists each recipient's messages in its own JSON file.
type Store struct {
	mu  sync.Mutex
	dir string
}

// New creates dir (if needed) and returns a ready store.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// path maps a recipient id to its inbox file, rejecting unsafe ids.
func (s *Store) path(to string) (string, error) {
	if err := store.SafeID(to); err != nil {
		return "", ErrBadRecipient
	}
	return filepath.Join(s.dir, to+".json"), nil
}

// load reads a recipient's messages; a missing file is an empty slice.
func (s *Store) load(path string) ([]Message, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []Message{}, nil
	}
	if err != nil {
		return nil, err
	}
	var ms []Message
	if err := json.Unmarshal(data, &ms); err != nil {
		return nil, err
	}
	return ms, nil
}

// save writes messages via temp file + rename so readers never see a partial file.
func (s *Store) save(path string, ms []Message) error {
	data, err := json.MarshalIndent(ms, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Append stores m in m.To's inbox, assigning a per-inbox sequential ID and TS.
func (s *Store) Append(m Message) (Message, error) {
	path, err := s.path(m.To)
	if err != nil {
		return Message{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, err := s.load(path)
	if err != nil {
		return Message{}, err
	}
	m.ID = strconv.Itoa(len(ms) + 1)
	m.TS = time.Now().UTC()
	m.Read = false
	ms = append(ms, m)
	if err := s.save(path, ms); err != nil {
		return Message{}, err
	}
	return m, nil
}

// Messages returns to's inbox in arrival order (read-only). Always non-nil.
func (s *Store) Messages(to string) ([]Message, error) {
	path, err := s.path(to)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(path)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/mailbox/...`
Expected: PASS (the 5 tests written above; `MarkRead`/`TakeFirstUnread` come in Task 2).

- [ ] **Step 5: Commit**

```bash
git add internal/mailbox/mailbox.go internal/mailbox/mailbox_test.go
git commit -m "feat(mailbox): per-recipient message store with Append/Messages"
```

---

## Task 2: mailbox package — MarkRead & TakeFirstUnread

**Files:**
- Modify: `internal/mailbox/mailbox.go`
- Test: `internal/mailbox/mailbox_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/mailbox/mailbox_test.go`:

```go
func TestMarkRead(t *testing.T) {
	s, _ := New(t.TempDir())
	m1, _ := s.Append(Message{To: "a", From: "x", Body: "1"})
	s.Append(Message{To: "a", From: "x", Body: "2"})

	if err := s.MarkRead("a", []string{m1.ID}); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	got, _ := s.Messages("a")
	if !got[0].Read || got[1].Read {
		t.Fatalf("read flags wrong: %+v", got)
	}
}

func TestTakeFirstUnreadOrderAndMarks(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Append(Message{To: "a", From: "x", Body: "first"})
	s.Append(Message{To: "a", From: "x", Body: "second"})

	m, ok, err := s.TakeFirstUnread("a", "")
	if err != nil || !ok || m.Body != "first" {
		t.Fatalf("first take: ok=%v m=%+v err=%v", ok, m, err)
	}
	// taken message is now read; next take returns the second
	m2, ok, _ := s.TakeFirstUnread("a", "")
	if !ok || m2.Body != "second" {
		t.Fatalf("second take: ok=%v m=%+v", ok, m2)
	}
	// nothing unread left
	if _, ok, _ := s.TakeFirstUnread("a", ""); ok {
		t.Fatalf("expected no unread left")
	}
}

func TestTakeFirstUnreadFromFilter(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Append(Message{To: "a", From: "x", Body: "from-x"})
	s.Append(Message{To: "a", From: "y", Body: "from-y"})

	m, ok, _ := s.TakeFirstUnread("a", "y")
	if !ok || m.Body != "from-y" {
		t.Fatalf("from filter: ok=%v m=%+v", ok, m)
	}
	// x's message is still unread (filter skipped it without consuming)
	m2, ok, _ := s.TakeFirstUnread("a", "x")
	if !ok || m2.Body != "from-x" {
		t.Fatalf("x still unread expected: ok=%v m=%+v", ok, m2)
	}
}

func TestTakeFirstUnreadEmpty(t *testing.T) {
	s, _ := New(t.TempDir())
	if _, ok, err := s.TakeFirstUnread("nobody", ""); ok || err != nil {
		t.Fatalf("want (false,nil), got ok=%v err=%v", ok, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mailbox/...`
Expected: FAIL — `s.MarkRead undefined` / `s.TakeFirstUnread undefined`.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/mailbox/mailbox.go`:

```go
// MarkRead flags the given message IDs read in to's inbox. Unknown IDs are
// ignored; a no-op (nothing changed) avoids a rewrite.
func (s *Store) MarkRead(to string, ids []string) error {
	path, err := s.path(to)
	if err != nil {
		return err
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, err := s.load(path)
	if err != nil {
		return err
	}
	changed := false
	for i := range ms {
		if want[ms[i].ID] && !ms[i].Read {
			ms[i].Read = true
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.save(path, ms)
}

// TakeFirstUnread atomically finds the oldest unread message in to's inbox
// matching from ("" = any sender), marks it read, and returns it. ok is false
// when nothing matches.
func (s *Store) TakeFirstUnread(to, from string) (Message, bool, error) {
	path, err := s.path(to)
	if err != nil {
		return Message{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, err := s.load(path)
	if err != nil {
		return Message{}, false, err
	}
	for i := range ms {
		if ms[i].Read {
			continue
		}
		if from != "" && ms[i].From != from {
			continue
		}
		ms[i].Read = true
		if err := s.save(path, ms); err != nil {
			return Message{}, false, err
		}
		return ms[i], true, nil
	}
	return Message{}, false, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/mailbox/...`
Expected: PASS (all tests).

- [ ] **Step 5: Commit**

```bash
git add internal/mailbox/mailbox.go internal/mailbox/mailbox_test.go
git commit -m "feat(mailbox): MarkRead and atomic TakeFirstUnread"
```

---

## Task 3: Daemon send + inbox routes

**Files:**
- Create: `internal/daemon/messages_routes.go`
- Modify: `internal/daemon/api.go` (add `mbox` field to `Server`; register routes in `router()`)
- Test: `internal/daemon/messages_routes_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/daemon/messages_routes_test.go`:

```go
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

	resp, _ := http.Post(ts.URL+"/sessions/busy/messages", "application/json", strings.NewReader(`{"from":"x","body":"hi"}`))
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
	resp, _ := http.Post(ts.URL+"/sessions/ghost/messages", "application/json", strings.NewReader(`{"body":"hi"}`))
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
	resp, _ := http.Post(ts.URL+"/sessions/agent-1/messages", "application/json", bytes.NewBufferString(`{"from":"x","body":""}`))
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run 'TestSendMessage|TestInbox'`
Expected: FAIL — `unknown field 'mbox' in struct literal of type Server`.

- [ ] **Step 3a: Add the `mbox` field to the Server struct**

In `internal/daemon/api.go`, add to the `Server` struct after the `cstore` field added in Phase 1:

```go
	// cstore is the shared-context KV store (the inter-agent blackboard).
	cstore *ctxstore.Store
	// mbox is the directed-message inbox store.
	mbox *mailbox.Store
}
```

Add the import to `internal/daemon/api.go`:

```go
	"github.com/srajanpathak/agentctl/internal/mailbox"
```

- [ ] **Step 3b: Register the message routes**

In `internal/daemon/api.go`, in `router()`, add the registration after `s.registerContextRoutes(r)` and before `s.registerStatic(r)`:

```go
	s.registerContextRoutes(r)
	s.registerMessageRoutes(r)
	s.registerStatic(r) // catch-all; must be last
```

- [ ] **Step 3c: Write the send + inbox handlers**

Create `internal/daemon/messages_routes.go`:

```go
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/srajanpathak/agentctl/internal/mailbox"
	"github.com/srajanpathak/agentctl/internal/store"
)

// sendMessageRequest is the body for POST /sessions/{id}/messages.
type sendMessageRequest struct {
	From string `json:"from"` // "" -> "human"
	Body string `json:"body"`
}

// sendMessageResponse is the body for POST /sessions/{id}/messages.
type sendMessageResponse struct {
	Message mailbox.Message `json:"message"`
	Woke    bool            `json:"woke"`
}

// inboxResponse is the body for GET /sessions/{id}/messages.
type inboxResponse struct {
	Messages []mailbox.Message `json:"messages"`
}

// parked reports whether a recipient is safe to wake with an injected notice.
// A working/spawning agent is NEVER interrupted (the message waits in the inbox).
func parked(st store.Status) bool {
	return st == store.StatusIdle || st == store.StatusWaitingForInput
}

func (s *Server) registerMessageRoutes(r chi.Router) {
	r.Post("/sessions/{id}/messages", s.handleSendMessage)
	r.Get("/sessions/{id}/messages", s.handleInbox)
}

func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req sendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Body == "" {
		writeErr(w, http.StatusBadRequest, "empty message body")
		return
	}
	from := req.From
	if from == "" {
		from = "human"
	}
	m, err := s.mbox.Append(mailbox.Message{To: id, From: from, Body: req.Body})
	if errors.Is(err, mailbox.ErrBadRecipient) {
		writeErr(w, http.StatusBadRequest, "invalid recipient")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	woke := false
	if parked(sess.Status) {
		notice := fmt.Sprintf("📨 New message from %s. Run `agentctl msg inbox` to read.", from)
		if err := s.life.Input(r.Context(), sess.TmuxSession, notice); err == nil {
			woke = true
		}
	}
	s.notify() // release any blocking waiters
	writeJSON(w, http.StatusCreated, sendMessageResponse{Message: m, Woke: woke})
}

func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	unreadOnly := r.URL.Query().Get("unread") == "true"
	msgs, err := s.mbox.Messages(id)
	if errors.Is(err, mailbox.ErrBadRecipient) {
		writeErr(w, http.StatusBadRequest, "invalid recipient")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []mailbox.Message{}
	ids := []string{}
	for _, m := range msgs {
		if unreadOnly && m.Read {
			continue
		}
		out = append(out, m)
		if !m.Read {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) > 0 {
		if err := s.mbox.MarkRead(id, ids); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, inboxResponse{Messages: out})
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/ -run 'TestSendMessage|TestInbox'`
Expected: PASS (all 5).

Then the whole daemon suite still builds/passes:
Run: `go test ./internal/daemon/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/messages_routes.go internal/daemon/messages_routes_test.go internal/daemon/api.go
git commit -m "feat(daemon): send + inbox message routes (wake only when parked)"
```

---

## Task 4: Daemon blocking wait route (long-poll)

**Files:**
- Create: `internal/daemon/messages_wait.go`
- Modify: `internal/daemon/messages_routes.go` (add the wait route to `registerMessageRoutes`)
- Test: `internal/daemon/messages_wait_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/daemon/messages_wait_test.go`:

```go
package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/srajanpathak/agentctl/internal/mailbox"
	"github.com/srajanpathak/agentctl/internal/store"
)

func TestWaitReturnsExistingMessage(t *testing.T) {
	srv, _, mb := newMsgServer(t)
	mb.Append(mailbox.Message{To: "agent-1", From: "agent-2", Body: "ready"})
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/sessions/agent-1/messages/wait?timeout=2")
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
	resp, _ := http.Get(ts.URL + "/sessions/empty/messages/wait?timeout=1")
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
		resp, _ := http.Get(ts.URL + "/sessions/agent-1/messages/wait?timeout=5")
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
	http.Post(ts.URL+"/sessions/agent-1/messages", "application/json", strings.NewReader(`{"from":"agent-2","body":"ping"}`))

	select {
	case got := <-done:
		if got != "ping" {
			t.Fatalf("want ping, got %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wait did not return after a message was delivered")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestWait`
Expected: FAIL — build error: `s.handleWaitMessage undefined` (the wait route/handler don't exist yet).

- [ ] **Step 3a: Add the wait route**

In `internal/daemon/messages_routes.go`, add the wait route to `registerMessageRoutes`:

```go
func (s *Server) registerMessageRoutes(r chi.Router) {
	r.Post("/sessions/{id}/messages", s.handleSendMessage)
	r.Get("/sessions/{id}/messages", s.handleInbox)
	r.Get("/sessions/{id}/messages/wait", s.handleWaitMessage)
}
```

- [ ] **Step 3b: Write the wait handler**

Create `internal/daemon/messages_wait.go`:

```go
package daemon

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/srajanpathak/agentctl/internal/mailbox"
)

const (
	defaultWaitSec = 300 // 5 min default long-poll window
	maxWaitSec     = 600 // hard cap
)

// waitResponse is the body for GET /sessions/{id}/messages/wait.
type waitResponse struct {
	Found   bool             `json:"found"`
	Message *mailbox.Message `json:"message,omitempty"`
}

// handleWaitMessage long-polls: it subscribes to the hub and returns as soon as
// an unread message for {id} (optionally filtered by ?from=) arrives, or returns
// found=false when ?timeout= (default 300s, capped 600s) elapses. This is what
// lets an agent await a reply in a single Bash call with no LLM-turn busy-poll.
func (s *Server) handleWaitMessage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	from := r.URL.Query().Get("from")
	timeoutSec := defaultWaitSec
	if q := r.URL.Query().Get("timeout"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			timeoutSec = n
		}
	}
	if timeoutSec > maxWaitSec {
		timeoutSec = maxWaitSec
	}

	ch, unsub := s.hub.subscribe()
	defer unsub()

	deadline := time.NewTimer(time.Duration(timeoutSec) * time.Second)
	defer deadline.Stop()

	for {
		m, ok, err := s.mbox.TakeFirstUnread(id, from)
		if errors.Is(err, mailbox.ErrBadRecipient) {
			writeErr(w, http.StatusBadRequest, "invalid recipient")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if ok {
			writeJSON(w, http.StatusOK, waitResponse{Found: true, Message: &m})
			return
		}
		select {
		case <-r.Context().Done():
			return // client hung up
		case <-s.done:
			writeJSON(w, http.StatusOK, waitResponse{Found: false})
			return
		case <-deadline.C:
			writeJSON(w, http.StatusOK, waitResponse{Found: false})
			return
		case <-ch:
			// something changed — loop and re-check the inbox
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/ -run TestWait`
Expected: PASS (all 3, including the ~1s timeout test and the goroutine wake test).

Then the whole daemon suite:
Run: `go test ./internal/daemon/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/messages_wait.go internal/daemon/messages_routes.go internal/daemon/messages_wait_test.go
git commit -m "feat(daemon): blocking message wait route (hub long-poll)"
```

---

## Task 5: Wire mailbox into NewServer and the daemon command

**Files:**
- Modify: `internal/daemon/server.go`
- Modify: `internal/cli/daemon.go`
- Modify: `internal/daemon/server_test.go`

- [ ] **Step 1: Update `NewServer` to accept the mailbox store**

In `internal/daemon/server.go`, add the import `"github.com/srajanpathak/agentctl/internal/mailbox"`, add `mbox *mailbox.Store` as the LAST parameter, and set `mbox: mbox` in the returned struct:

```go
func NewServer(st store.Store, life Lifecycle, p *poller.Poller, interval time.Duration, approvals bool, cstore *ctxstore.Store, mbox *mailbox.Store) *Server {
	h := newHub()
	if p != nil {
		p.OnChange = h.publish
	}
	return &Server{
		store: st, life: life, poller: p, pollInterval: interval,
		hub: h, done: make(chan struct{}), approvals: approvals, cstore: cstore, mbox: mbox,
	}
}
```

- [ ] **Step 2: Run build to verify it fails at the call sites**

Run: `go build ./...`
Expected: FAIL — `not enough arguments in call to daemon.NewServer` at `internal/cli/daemon.go` and `internal/daemon/server_test.go`.

- [ ] **Step 3: Update the two call sites**

In `internal/cli/daemon.go`, add the import `"github.com/srajanpathak/agentctl/internal/mailbox"`, construct the store after the ctxstore block, and pass it:

```go
			cstore, err := ctxstore.New(filepath.Join(cfg.DataDir, "context"))
			if err != nil {
				return err
			}

			mbox, err := mailbox.New(filepath.Join(cfg.DataDir, "inbox"))
			if err != nil {
				return err
			}
```

and change the `NewServer` line:

```go
			srv := daemon.NewServer(st, life, pl, 10*time.Second, cfg.ApprovalsEnabled, cstore, mbox)
```

In `internal/daemon/server_test.go`, update the `NewServer(...)` call to pass `nil` for the new arg:

```go
	srv := NewServer(newFakeStore(), &fakeLife{}, nil, time.Second, false, nil, nil)
```

- [ ] **Step 4: Run build and tests**

Run: `go build ./... && go test ./internal/daemon/... ./internal/cli/...`
Expected: PASS, no build errors.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/server.go internal/cli/daemon.go internal/daemon/server_test.go
git commit -m "feat(daemon): construct and inject mailbox store in NewServer + daemon cmd"
```

---

## Task 6: Client methods for messages

**Files:**
- Modify: `internal/client/client.go`
- Test: `internal/client/client_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/client/client_test.go`:

```go
func TestMsgSendParsesMessageAndWoke(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte(`{"message":{"id":"1","from":"agent-2","to":"agent-1","body":"hi"},"woke":true}`))
	}))
	defer ts.Close()

	m, woke, err := New(ts.URL).MsgSend(context.Background(), "agent-1", "agent-2", "hi")
	if err != nil {
		t.Fatalf("MsgSend: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/sessions/agent-1/messages" {
		t.Fatalf("got %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"from":"agent-2"`) || !strings.Contains(gotBody, `"body":"hi"`) {
		t.Fatalf("body=%s", gotBody)
	}
	if m.ID != "1" || !woke {
		t.Fatalf("m=%+v woke=%v", m, woke)
	}
}

func TestMsgInboxForwardsUnread(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("unread") != "true" {
			t.Errorf("unread not forwarded")
		}
		w.Write([]byte(`{"messages":[{"id":"1","from":"x","body":"a"}]}`))
	}))
	defer ts.Close()

	got, err := New(ts.URL).MsgInbox(context.Background(), "agent-1", true)
	if err != nil {
		t.Fatalf("MsgInbox: %v", err)
	}
	if len(got) != 1 || got[0].Body != "a" {
		t.Fatalf("got %+v", got)
	}
}

func TestMsgWaitFoundAndTimeout(t *testing.T) {
	// found
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("timeout") != "1" || r.URL.Query().Get("from") != "agent-2" {
			t.Errorf("query not forwarded: %s", r.URL.RawQuery)
		}
		w.Write([]byte(`{"found":true,"message":{"id":"1","from":"agent-2","body":"reply"}}`))
	}))
	m, err := New(ts.URL).MsgWait(context.Background(), "agent-1", "agent-2", 1)
	ts.Close()
	if err != nil || m == nil || m.Body != "reply" {
		t.Fatalf("found case: m=%+v err=%v", m, err)
	}

	// timeout
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"found":false}`))
	}))
	defer ts2.Close()
	m2, err := New(ts2.URL).MsgWait(context.Background(), "agent-1", "", 1)
	if err != nil || m2 != nil {
		t.Fatalf("timeout case: m=%+v err=%v", m2, err)
	}
}
```

(`io`, `strings`, `context`, `net/http`, `net/http/httptest` are already imported in `client_test.go` from Phase 1.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/client/ -run TestMsg`
Expected: FAIL — `c.MsgSend undefined` / `c.MsgInbox undefined` / `c.MsgWait undefined`.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/client/client.go` (`fmt`, `net/url`, `net/http`, `time`, `context` are already imported):

```go
// Message mirrors the daemon's mailbox message (directed messages).
type Message struct {
	ID   string    `json:"id"`
	From string    `json:"from"`
	To   string    `json:"to"`
	Body string    `json:"body"`
	TS   time.Time `json:"ts"`
	Read bool      `json:"read"`
}

// MsgSend delivers body to recipient `to` from `from`; returns the stored
// message and whether the recipient was woken.
func (c *Client) MsgSend(ctx context.Context, to, from, body string) (Message, bool, error) {
	var resp struct {
		Message Message `json:"message"`
		Woke    bool    `json:"woke"`
	}
	reqBody := map[string]string{"from": from, "body": body}
	if err := c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(to)+"/messages", reqBody, &resp); err != nil {
		return Message{}, false, err
	}
	return resp.Message, resp.Woke, nil
}

// MsgInbox returns id's messages (unreadOnly filters to unread); the daemon
// marks the returned messages read.
func (c *Client) MsgInbox(ctx context.Context, id string, unreadOnly bool) ([]Message, error) {
	p := "/sessions/" + url.PathEscape(id) + "/messages"
	if unreadOnly {
		p += "?unread=true"
	}
	var resp struct {
		Messages []Message `json:"messages"`
	}
	if err := c.do(ctx, http.MethodGet, p, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Messages, nil
}

// MsgWait blocks (server long-poll) until a message for id arrives (optionally
// filtered by sender `from`) or timeoutSec elapses. Returns nil on timeout. The
// HTTP deadline is set beyond the server window so the client never cuts the
// long-poll short.
func (c *Client) MsgWait(ctx context.Context, id, from string, timeoutSec int) (*Message, error) {
	p := fmt.Sprintf("/sessions/%s/messages/wait?timeout=%d", url.PathEscape(id), timeoutSec)
	if from != "" {
		p += "&from=" + url.QueryEscape(from)
	}
	var resp struct {
		Found   bool     `json:"found"`
		Message *Message `json:"message"`
	}
	if err := c.doT(ctx, time.Duration(timeoutSec+10)*time.Second, http.MethodGet, p, nil, &resp); err != nil {
		return nil, err
	}
	if !resp.Found {
		return nil, nil
	}
	return resp.Message, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/client/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/client/client.go internal/client/client_test.go
git commit -m "feat(client): MsgSend/MsgInbox/MsgWait"
```

---

## Task 7: CLI `msg` command group

**Files:**
- Create: `internal/cli/messages.go`
- Test: `internal/cli/messages_test.go`
- Modify: `internal/cli/root.go`

- [ ] **Step 1: Write the failing test**

Create `internal/cli/messages_test.go`:

```go
package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/srajanpathak/agentctl/internal/client"
)

func TestResolveSenderPrecedence(t *testing.T) {
	if resolveSender("flag", "env") != "flag" {
		t.Fatal("--as should win")
	}
	if resolveSender("", "env") != "env" {
		t.Fatal("env next")
	}
	if resolveSender("", "") != "human" {
		t.Fatal("default human")
	}
}

func TestResolveSelfRequiresID(t *testing.T) {
	if v, err := resolveSelf("flag", "env"); err != nil || v != "flag" {
		t.Fatalf("--as: v=%q err=%v", v, err)
	}
	if v, err := resolveSelf("", "env"); err != nil || v != "env" {
		t.Fatalf("env: v=%q err=%v", v, err)
	}
	if _, err := resolveSelf("", ""); err == nil {
		t.Fatal("expected error when no id available")
	}
}

func TestFormatMessage(t *testing.T) {
	m := client.Message{From: "agent-A", Body: "hi there", Read: false,
		TS: time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)}
	out := formatMessage(m)
	if !strings.Contains(out, "agent-A") || !strings.Contains(out, "hi there") || !strings.Contains(out, "[unread]") {
		t.Fatalf("got %q", out)
	}
	read := client.Message{From: "agent-A", Body: "x", Read: true, TS: m.TS}
	if strings.Contains(formatMessage(read), "[unread]") {
		t.Fatalf("read message should not show [unread]")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run 'TestResolve|TestFormatMessage'`
Expected: FAIL — `undefined: resolveSender` / `resolveSelf` / `formatMessage`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/cli/messages.go`:

```go
package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/srajanpathak/agentctl/internal/client"
)

// resolveSender picks the "from" identity for outgoing messages: --as, else the
// env id, else "human".
func resolveSender(asFlag, env string) string {
	if asFlag != "" {
		return asFlag
	}
	if env != "" {
		return env
	}
	return "human"
}

// resolveSelf picks WHOSE inbox to read: --as, else the env id; errors if
// neither is set (there is no sensible default recipient).
func resolveSelf(asFlag, env string) (string, error) {
	if asFlag != "" {
		return asFlag, nil
	}
	if env != "" {
		return env, nil
	}
	return "", fmt.Errorf("no agent id: pass --as <id> or set AGENTCTL_SESSION_ID")
}

func formatMessage(m client.Message) string {
	flag := ""
	if !m.Read {
		flag = " [unread]"
	}
	return fmt.Sprintf("from %s at %s%s\n  %s", m.From, m.TS.Format(time.RFC3339), flag, m.Body)
}

func newMsgCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "msg",
		Short: "Send and receive directed messages between agents",
	}
	cmd.PersistentFlags().String("as", "", "act as this agent id (defaults to $AGENTCTL_SESSION_ID)")
	cmd.AddCommand(newMsgSendCmd(), newMsgInboxCmd(), newMsgWaitCmd())
	return cmd
}

func newMsgSendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "send <to> <message...>",
		Short: "Send a message to an agent (wakes it if it's idle/waiting)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			as, _ := cmd.Flags().GetString("as")
			from := resolveSender(as, os.Getenv("AGENTCTL_SESSION_ID"))
			to, body := args[0], strings.Join(args[1:], " ")
			m, woke, err := clientFor(cmd).MsgSend(cmd.Context(), to, from, body)
			if err != nil {
				return err
			}
			out := fmt.Sprintf("sent to %s (id %s)", to, m.ID)
			if woke {
				out += " — woke recipient"
			}
			fmt.Fprintln(cmd.OutOrStdout(), out)
			return nil
		},
	}
}

func newMsgInboxCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inbox",
		Short: "Show this agent's messages (marks them read)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			as, _ := cmd.Flags().GetString("as")
			self, err := resolveSelf(as, os.Getenv("AGENTCTL_SESSION_ID"))
			if err != nil {
				return err
			}
			unread, _ := cmd.Flags().GetBool("unread")
			msgs, err := clientFor(cmd).MsgInbox(cmd.Context(), self, unread)
			if err != nil {
				return err
			}
			if len(msgs) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no messages)")
				return nil
			}
			for _, m := range msgs {
				fmt.Fprintln(cmd.OutOrStdout(), formatMessage(m))
			}
			return nil
		},
	}
	cmd.Flags().Bool("unread", false, "show only unread messages")
	return cmd
}

func newMsgWaitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wait",
		Short: "Block until a message arrives (or timeout), then print it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			as, _ := cmd.Flags().GetString("as")
			self, err := resolveSelf(as, os.Getenv("AGENTCTL_SESSION_ID"))
			if err != nil {
				return err
			}
			from, _ := cmd.Flags().GetString("from")
			timeout, _ := cmd.Flags().GetInt("timeout")
			m, err := clientFor(cmd).MsgWait(cmd.Context(), self, from, timeout)
			if err != nil {
				return err
			}
			if m == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "(no message — timed out)")
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), formatMessage(*m))
			return nil
		},
	}
	cmd.Flags().String("from", "", "only wait for a message from this sender")
	cmd.Flags().Int("timeout", 300, "seconds to wait before giving up")
	return cmd
}
```

- [ ] **Step 4: Register the command**

In `internal/cli/root.go`, add to the `AddCommand` group (right after the `newCtxCmd()` line added in Phase 1):

```go
	root.AddCommand(newCtxCmd())
	root.AddCommand(newMsgCmd())
```

- [ ] **Step 5: Run test + build to verify it passes**

Run: `go test ./internal/cli/... && go build ./...`
Expected: PASS, no build errors.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/messages.go internal/cli/messages_test.go internal/cli/root.go
git commit -m "feat(cli): msg send/inbox/wait command group"
```

---

## Task 8: MCP tools for messages

**Files:**
- Modify: `internal/mcp/server.go`

**Note on this task's shape (same as the Phase 1 MCP task):** The existing `internal/mcp/server_test.go` tests assert the CLIENT PATH a tool wraps (they do not dispatch tools through the SDK); tool registration is verified by `go build`. We add `send_message` and `read_inbox` only — NOT a wait tool, because a blocking long-poll behind an MCP call would hit MCP-client timeouts; waiting is intentionally a CLI/Bash concern.

- [ ] **Step 1: Write the guard test**

Append to `internal/mcp/server_test.go`:

```go
func TestSendMessageClientPath(t *testing.T) {
	var gotPath, gotMethod string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Write([]byte(`{"message":{"id":"1","from":"agent","to":"agent-1","body":"hi"},"woke":false}`))
	}))
	defer daemon.Close()

	srv := NewServer(daemon.URL)
	if _, _, err := srv.cl.MsgSend(context.Background(), "agent-1", "agent", "hi"); err != nil {
		t.Fatalf("MsgSend: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/sessions/agent-1/messages" {
		t.Fatalf("got %s %s", gotMethod, gotPath)
	}
}
```

(`context`, `net/http`, `net/http/httptest` are already imported in that test file.)

- [ ] **Step 2: Run the test**

Run: `go test ./internal/mcp/ -run TestSendMessageClientPath`
Expected: PASS (asserts the client path the tool wraps; the client method exists from Task 6).

- [ ] **Step 3: Add the arg structs and tools**

In `internal/mcp/server.go`, add the arg structs near the other arg types:

```go
type sendMessageArgs struct {
	To   string `json:"to" jsonschema:"recipient agent's session id"`
	Body string `json:"body" jsonschema:"the message text"`
}
type readInboxArgs struct {
	Agent  string `json:"agent,omitempty" jsonschema:"whose inbox to read; defaults to this agent ($AGENTCTL_SESSION_ID)"`
	Unread bool   `json:"unread,omitempty" jsonschema:"only return unread messages"`
}
```

Register the two tools inside `NewServer` (after the `ctx_list` tool block added in Phase 1, before `terminate_agent`). Note: reuse the existing `ctxWriter()` helper (already in this file — returns `$AGENTCTL_SESSION_ID` or `"agent"`) for the sender identity. `os` is already imported.

```go
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "send_message",
		Description: "Send a directed message to another agent's inbox (wakes it only if it's idle/waiting). Use for peer consultation or handoff signals.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a sendMessageArgs) (*mcpsdk.CallToolResult, any, error) {
		m, woke, err := s.cl.MsgSend(ctx, a.To, ctxWriter(), a.Body)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		msg := "sent to " + a.To + " (id " + m.ID + ")"
		if woke {
			msg += " — woke recipient"
		}
		return textResult(msg), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "read_inbox",
		Description: "Read directed messages addressed to this agent (marks them read).",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a readInboxArgs) (*mcpsdk.CallToolResult, any, error) {
		who := a.Agent
		if who == "" {
			who = os.Getenv("AGENTCTL_SESSION_ID")
		}
		if who == "" {
			return textResult("error: no agent id — set AGENTCTL_SESSION_ID or pass `agent`"), nil, nil
		}
		msgs, err := s.cl.MsgInbox(ctx, who, a.Unread)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		res, err := jsonResult(msgs)
		return res, nil, err
	})
```

- [ ] **Step 4: Run tests + build to verify they pass**

Run: `go test ./internal/mcp/... && go build ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/mcp/server.go internal/mcp/server_test.go
git commit -m "feat(mcp): send_message and read_inbox tools"
```

---

## Task 9: Document the `msg` commands + final verification

**Files:**
- Modify: `docs/USAGE.md`

- [ ] **Step 1: Add a directed-messages section**

FIRST read `docs/USAGE.md` to match its heading style (the Phase 1 "Shared context" section is a good template — numbered `##`, fenced ```` ```sh ```` blocks, `---` rule). Append a "Directed messages" section conveying:

```markdown
## Directed messages

Agent-to-agent messages with a durable per-recipient inbox.

    agentctl msg send <agent-id> "can you check the auth module?"   # deliver + wake if idle
    agentctl msg inbox                                              # read my messages (marks read)
    agentctl msg inbox --unread                                     # only unread
    agentctl msg wait --from <agent-id> --timeout 120               # block until a reply (one call)

Sending **wakes the recipient only if it's idle or waiting** — a working agent is
never interrupted; its message waits in the inbox. `msg wait` blocks in the
daemon (a long-poll), so an agent awaits a reply in a single call with no
busy-loop. Identity defaults to `$AGENTCTL_SESSION_ID` (set per agent in a later
phase); until then pass `--as <agent-id>`. Also available as MCP tools
`send_message` / `read_inbox` (no MCP `wait` — use the CLI for blocking waits).

Request/reply pattern: A runs `msg send B "..."` then `msg wait --from B`; B reads
its inbox, does the work, and replies with `msg send A "..."`, unblocking A.
```

- [ ] **Step 2: Run the full suite (including -race on the new packages)**

Run: `go build ./... && go test ./... && go test -race ./internal/mailbox/... ./internal/daemon/... && make lint`
Expected: PASS across all packages; lint (go vet) clean. If ANYTHING fails, do NOT commit — report it.

- [ ] **Step 3: Commit**

```bash
git add docs/USAGE.md
git commit -m "docs: document agentctl msg directed-message commands"
```

---

## Verification checklist (after all tasks)

- [ ] `go build ./...` clean.
- [ ] `go test ./...` green; `go test -race ./internal/mailbox/... ./internal/daemon/...` green.
- [ ] `make lint` clean.
- [ ] Manual smoke (rebuild + restart daemon: `./scripts/reinstall.sh`), simulating two agents by id:
  - `agentctl msg send <real-agent-id> "hello"` → `sent to <id> (id 1)` (with "— woke recipient" if that agent is idle/waiting).
  - `agentctl msg inbox --as <real-agent-id>` → shows the message; a second call with `--unread` shows nothing.
  - Blocking wait demo (two terminals): terminal 1 `agentctl msg wait --as agentA --from agentB --timeout 60` (blocks); terminal 2 `agentctl msg send agentA "pong" --as agentB` → terminal 1 unblocks and prints the message. (`agentA`/`agentB` must be real session ids; use `agentctl ls` to pick two.)
  - Note: wake injection only fires for a real, parked tmux session; for ad-hoc id testing the inbox + wait paths work regardless.
```
