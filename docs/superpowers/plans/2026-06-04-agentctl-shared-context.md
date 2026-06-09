# Shared Context (Phase 1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a daemon-owned, namespaced key/value store ("shared context") that agents read and write via the agentctl CLI, HTTP API, and MCP — the foundational substrate for inter-agent communication and pipelines.

**Architecture:** A new pure `internal/ctxstore` package persists all entries in a single JSON file (`<data-dir>/context/context.json`), rewritten atomically under an `RWMutex` (localhost session-store scale — mirrors `internal/store/file.go`'s approach). The daemon holds a `*ctxstore.Store`, exposes `PUT/GET/DELETE /context/{key}` and `GET /context?prefix=`, the client wraps those, the CLI adds an `agentctl ctx set/get/list/del` command group, and MCP mirrors `ctx_set/ctx_get/ctx_list`.

**Tech Stack:** Go, chi router (`github.com/go-chi/chi/v5`), cobra CLI (`github.com/spf13/cobra`), MCP SDK (`github.com/modelcontextprotocol/go-sdk/mcp`). Module: `github.com/srajanpathak/agentctl`.

**Design decision (deviation from spec §5.1):** The spec suggested "one JSON file per namespace." This plan uses a **single file** holding `map[key]Entry`. It is simpler, fully correct, and makes the future prefix-delete (pipeline cleanup, Phase 4) *easier* (delete matching keys + one atomic rewrite) rather than harder. Per-namespace file splitting can be revisited if profiling ever demands it.

---

## File Structure

- **Create** `internal/ctxstore/ctxstore.go` — the KV store: `Entry`, `Store`, `New`, `Set`, `Get`, `List`, `Del`, errors. One responsibility: persist/retrieve namespaced values.
- **Create** `internal/ctxstore/ctxstore_test.go` — table tests for the store.
- **Create** `internal/daemon/context_routes.go` — HTTP handlers + route registration for `/context`.
- **Create** `internal/daemon/context_routes_test.go` — httptest coverage of the handlers.
- **Modify** `internal/daemon/api.go` — add `cstore` field to `Server`; register context routes in `router()`.
- **Modify** `internal/daemon/server.go` — add `*ctxstore.Store` param to `NewServer`.
- **Modify** `internal/cli/daemon.go` — construct the `ctxstore.Store` and pass it to `NewServer`.
- **Modify** `internal/daemon/server_test.go` — update the `NewServer(...)` call.
- **Modify** `internal/client/client.go` — add `ContextEntry` type + `CtxSet/CtxGet/CtxList/CtxDel`.
- **Modify** `internal/client/client_test.go` — add a client round-trip test.
- **Create** `internal/cli/context.go` — the `ctx` command group + `resolveCtxValue` helper.
- **Create** `internal/cli/context_test.go` — unit test for `resolveCtxValue`.
- **Modify** `internal/cli/root.go` — register `newCtxCmd()`.
- **Modify** `internal/mcp/server.go` — add `ctx_set/ctx_get/ctx_list` tools + arg structs.
- **Modify** `docs/USAGE.md` — document the `ctx` commands.

---

## Task 1: ctxstore package — Set & Get

**Files:**
- Create: `internal/ctxstore/ctxstore.go`
- Test: `internal/ctxstore/ctxstore_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/ctxstore/ctxstore_test.go`:

```go
package ctxstore

import (
	"errors"
	"testing"
)

func TestSetThenGet(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Set("global.greeting", "hello", "agent-A"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	e, err := s.Get("global.greeting")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if e.Value != "hello" || e.UpdatedBy != "agent-A" || e.Key != "global.greeting" {
		t.Fatalf("got %+v", e)
	}
	if e.UpdatedAt.IsZero() {
		t.Fatalf("UpdatedAt not set")
	}
}

func TestGetMissingReturnsNotFound(t *testing.T) {
	s, _ := New(t.TempDir())
	if _, err := s.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSetEmptyKeyRejected(t *testing.T) {
	s, _ := New(t.TempDir())
	if _, err := s.Set("", "v", "by"); !errors.Is(err, ErrBadKey) {
		t.Fatalf("want ErrBadKey, got %v", err)
	}
}

func TestSetOverwrites(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Set("k", "v1", "a")
	s.Set("k", "v2", "b")
	e, _ := s.Get("k")
	if e.Value != "v2" || e.UpdatedBy != "b" {
		t.Fatalf("overwrite failed: %+v", e)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s1, _ := New(dir)
	s1.Set("k", "v", "a")
	s2, err := New(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	e, err := s2.Get("k")
	if err != nil || e.Value != "v" {
		t.Fatalf("not persisted: %+v err=%v", e, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ctxstore/...`
Expected: FAIL — `undefined: New` / package has no buildable files.

- [ ] **Step 3: Write minimal implementation**

Create `internal/ctxstore/ctxstore.go`:

```go
// Package ctxstore is a daemon-owned, namespaced key/value store that agents
// read and write to share results and state (the "shared context" / blackboard).
// Keys are free-form dot-namespaced strings (e.g. "global.foo",
// "pipeline.<pid>.<job>.output"). All entries live in one JSON file under the
// data dir, rewritten atomically (temp file + rename) on each mutation — this is
// a localhost session store, not a database; the last write surviving a crash is
// not a requirement, but a reader never observes a torn file.
package ctxstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned when a key does not exist.
var ErrNotFound = errors.New("context key not found")

// ErrBadKey is returned when a key is empty/blank.
var ErrBadKey = errors.New("invalid context key")

// Entry is one stored value plus its provenance.
type Entry struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store persists all entries in one JSON file, serialized by an RWMutex.
type Store struct {
	mu   sync.RWMutex
	path string
}

// New creates dir (if needed) and returns a store writing to <dir>/context.json.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{path: filepath.Join(dir, "context.json")}, nil
}

// load reads the whole map; a missing file is an empty map, not an error.
func (s *Store) load() (map[string]Entry, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Entry{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]Entry{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// save writes the whole map via temp file + rename so readers never see a
// partial write.
func (s *Store) save(m map[string]Entry) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".tmp-*")
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
	return os.Rename(tmpName, s.path)
}

// Set writes value at key, recording the writer (by) and current time.
func (s *Store) Set(key, value, by string) (Entry, error) {
	if strings.TrimSpace(key) == "" {
		return Entry{}, ErrBadKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return Entry{}, err
	}
	e := Entry{Key: key, Value: value, UpdatedBy: by, UpdatedAt: time.Now().UTC()}
	m[key] = e
	if err := s.save(m); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// Get returns the entry at key, or ErrNotFound.
func (s *Store) Get(key string) (Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, err := s.load()
	if err != nil {
		return Entry{}, err
	}
	e, ok := m[key]
	if !ok {
		return Entry{}, ErrNotFound
	}
	return e, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/ctxstore/...`
Expected: PASS (4 of the 5 tests; `TestListAndDel` is added in Task 2). All written tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/ctxstore/ctxstore.go internal/ctxstore/ctxstore_test.go
git commit -m "feat(ctxstore): shared context KV store with Set/Get"
```

---

## Task 2: ctxstore package — List & Del

**Files:**
- Modify: `internal/ctxstore/ctxstore.go`
- Test: `internal/ctxstore/ctxstore_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/ctxstore/ctxstore_test.go`:

```go
func TestListByPrefixSorted(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Set("pipeline.p1.b.output", "B", "b")
	s.Set("pipeline.p1.a.output", "A", "a")
	s.Set("global.x", "X", "x")

	all, err := s.List("")
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3, got %d", len(all))
	}
	// sorted by key: global.x, pipeline.p1.a.output, pipeline.p1.b.output
	if all[0].Key != "global.x" || all[1].Key != "pipeline.p1.a.output" {
		t.Fatalf("not sorted: %+v", all)
	}

	pref, _ := s.List("pipeline.p1.")
	if len(pref) != 2 {
		t.Fatalf("prefix want 2, got %d", len(pref))
	}
}

func TestListEmptyStoreReturnsEmptySlice(t *testing.T) {
	s, _ := New(t.TempDir())
	got, err := s.List("")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("want empty non-nil slice, got %#v", got)
	}
}

func TestDel(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Set("k", "v", "a")
	if err := s.Del("k"); err != nil {
		t.Fatalf("Del: %v", err)
	}
	if _, err := s.Get("k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("still present after Del")
	}
	if err := s.Del("k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Del missing want ErrNotFound, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ctxstore/...`
Expected: FAIL — `s.List undefined` and `s.Del undefined`.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/ctxstore/ctxstore.go`:

```go
// List returns all entries whose key starts with prefix (empty = all), sorted
// by key. Always returns a non-nil slice.
func (s *Store) List(prefix string) ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, err := s.load()
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	for k, e := range m {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Del removes key, returning ErrNotFound if it was absent.
func (s *Store) Del(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := m[key]; !ok {
		return ErrNotFound
	}
	delete(m, key)
	return s.save(m)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/ctxstore/...`
Expected: PASS (all tests).

- [ ] **Step 5: Commit**

```bash
git add internal/ctxstore/ctxstore.go internal/ctxstore/ctxstore_test.go
git commit -m "feat(ctxstore): List(prefix) and Del"
```

---

## Task 3: Daemon HTTP routes for /context

**Files:**
- Create: `internal/daemon/context_routes.go`
- Modify: `internal/daemon/api.go` (add `cstore` field to `Server`; register routes in `router()`)
- Test: `internal/daemon/context_routes_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/daemon/context_routes_test.go`:

```go
package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srajanpathak/agentctl/internal/ctxstore"
)

func newCtxTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cs, err := ctxstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("ctxstore.New: %v", err)
	}
	return httptest.NewServer((&Server{cstore: cs}).router())
}

func TestContextSetGetRoundTrip(t *testing.T) {
	ts := newCtxTestServer(t)
	defer ts.Close()

	body := bytes.NewBufferString(`{"value":"hello","by":"agent-A"}`)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/context/global.greeting", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, err = http.Get(ts.URL + "/context/global.greeting")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var e ctxstore.Entry
	json.NewDecoder(resp.Body).Decode(&e)
	if e.Value != "hello" || e.UpdatedBy != "agent-A" {
		t.Fatalf("got %+v", e)
	}
}

func TestContextGetMissing404(t *testing.T) {
	ts := newCtxTestServer(t)
	defer ts.Close()
	resp, _ := http.Get(ts.URL + "/context/missing")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

func TestContextSetDefaultsWriterToHuman(t *testing.T) {
	ts := newCtxTestServer(t)
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/context/k", bytes.NewBufferString(`{"value":"v"}`))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	resp, _ = http.Get(ts.URL + "/context/k")
	defer resp.Body.Close()
	var e ctxstore.Entry
	json.NewDecoder(resp.Body).Decode(&e)
	if e.UpdatedBy != "human" {
		t.Fatalf("want writer 'human', got %q", e.UpdatedBy)
	}
}

func TestContextListAndDelete(t *testing.T) {
	ts := newCtxTestServer(t)
	defer ts.Close()
	for _, k := range []string{"pipeline.p.a", "pipeline.p.b", "global.x"} {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/context/"+k, bytes.NewBufferString(`{"value":"v"}`))
		http.DefaultClient.Do(req)
	}

	resp, _ := http.Get(ts.URL + "/context?prefix=pipeline.p.")
	var lr struct {
		Entries []ctxstore.Entry `json:"entries"`
	}
	json.NewDecoder(resp.Body).Decode(&lr)
	resp.Body.Close()
	if len(lr.Entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(lr.Entries))
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/context/global.x", nil)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE status %d", resp.StatusCode)
	}
	resp, _ = http.Get(ts.URL + "/context/global.x")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 after delete, got %d", resp.StatusCode)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestContext`
Expected: FAIL — `unknown field 'cstore' in struct literal` (the field doesn't exist yet).

- [ ] **Step 3a: Add the `cstore` field to the Server struct**

In `internal/daemon/api.go`, in the `Server` struct (currently ending with the `approvals bool` field), add a field:

```go
	// approvals gates the approvals-inbox endpoints (AGENTCTL_APPROVALS).
	approvals bool
	// cstore is the shared-context KV store (the inter-agent blackboard).
	cstore *ctxstore.Store
}
```

Add the import to `internal/daemon/api.go`'s import block:

```go
	"github.com/srajanpathak/agentctl/internal/ctxstore"
```

- [ ] **Step 3b: Register the context routes**

In `internal/daemon/api.go`, in `router()`, add the registration line immediately after the approve route and before `s.registerStatic(r)`:

```go
	r.Get("/approvals", s.handleApprovals)
	r.Post("/sessions/{id}/approve", s.handleApprove)
	s.registerContextRoutes(r)
	s.registerStatic(r) // catch-all; must be last
```

- [ ] **Step 3c: Write the handlers**

Create `internal/daemon/context_routes.go`:

```go
package daemon

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/srajanpathak/agentctl/internal/ctxstore"
)

// ctxSetRequest is the body for PUT /context/{key}.
type ctxSetRequest struct {
	Value string `json:"value"`
	By    string `json:"by"` // writer identity; "" -> "human"
}

// ctxListResponse is the body for GET /context.
type ctxListResponse struct {
	Entries []ctxstore.Entry `json:"entries"`
}

func (s *Server) registerContextRoutes(r chi.Router) {
	r.Put("/context/{key}", s.handleCtxSet)
	r.Get("/context/{key}", s.handleCtxGet)
	r.Delete("/context/{key}", s.handleCtxDel)
	r.Get("/context", s.handleCtxList)
}

func (s *Server) handleCtxSet(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	var req ctxSetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	by := req.By
	if by == "" {
		by = "human"
	}
	e, err := s.cstore.Set(key, req.Value, by)
	if errors.Is(err, ctxstore.ErrBadKey) {
		writeErr(w, http.StatusBadRequest, "invalid key")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) handleCtxGet(w http.ResponseWriter, r *http.Request) {
	e, err := s.cstore.Get(chi.URLParam(r, "key"))
	if errors.Is(err, ctxstore.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "context key not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) handleCtxDel(w http.ResponseWriter, r *http.Request) {
	err := s.cstore.Del(chi.URLParam(r, "key"))
	if errors.Is(err, ctxstore.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "context key not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleCtxList(w http.ResponseWriter, r *http.Request) {
	entries, err := s.cstore.List(r.URL.Query().Get("prefix"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ctxListResponse{Entries: entries})
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/ -run TestContext`
Expected: PASS (all 4 context tests).

Then confirm the rest of the daemon suite still builds/passes:
Run: `go test ./internal/daemon/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/context_routes.go internal/daemon/context_routes_test.go internal/daemon/api.go
git commit -m "feat(daemon): /context HTTP routes backed by ctxstore"
```

---

## Task 4: Wire ctxstore into NewServer and the daemon command

**Files:**
- Modify: `internal/daemon/server.go` (add param to `NewServer`)
- Modify: `internal/cli/daemon.go` (construct + pass the store)
- Modify: `internal/daemon/server_test.go` (update the `NewServer` call)

- [ ] **Step 1: Update `NewServer` to accept the store**

In `internal/daemon/server.go`, change the signature and assignment. Add the import `"github.com/srajanpathak/agentctl/internal/ctxstore"`.

```go
func NewServer(st store.Store, life Lifecycle, p *poller.Poller, interval time.Duration, approvals bool, cstore *ctxstore.Store) *Server {
	h := newHub()
	if p != nil {
		p.OnChange = h.publish
	}
	return &Server{
		store: st, life: life, poller: p, pollInterval: interval,
		hub: h, done: make(chan struct{}), approvals: approvals, cstore: cstore,
	}
}
```

- [ ] **Step 2: Run build to verify it fails at call sites**

Run: `go build ./...`
Expected: FAIL — `not enough arguments in call to daemon.NewServer` at `internal/cli/daemon.go` and `internal/daemon/server_test.go`.

- [ ] **Step 3: Update the two call sites**

In `internal/cli/daemon.go`, add the import `"github.com/srajanpathak/agentctl/internal/ctxstore"`, construct the store after the file store, and pass it:

```go
			st, err := store.NewFileStore(cfg.DataDir)
			if err != nil {
				return err
			}
			defer st.Close(context.Background())

			cstore, err := ctxstore.New(filepath.Join(cfg.DataDir, "context"))
			if err != nil {
				return err
			}
```

and change the `NewServer` line:

```go
			srv := daemon.NewServer(st, life, pl, 10*time.Second, cfg.ApprovalsEnabled, cstore)
```

(`path/filepath` is already imported in `daemon.go`.)

In `internal/daemon/server_test.go:25`, update the call to pass `nil` for the store (this test does not exercise context routes):

```go
	srv := NewServer(newFakeStore(), &fakeLife{}, nil, time.Second, false, nil)
```

- [ ] **Step 4: Run build and full test suite**

Run: `go build ./... && go test ./internal/daemon/... ./internal/cli/...`
Expected: PASS, no build errors.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/server.go internal/cli/daemon.go internal/daemon/server_test.go
git commit -m "feat(daemon): construct and inject ctxstore in NewServer + daemon cmd"
```

---

## Task 5: Client methods for shared context

**Files:**
- Modify: `internal/client/client.go`
- Test: `internal/client/client_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/client/client_test.go`:

```go
func TestCtxSetSendsValueAndBy(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"key":"global.k","value":"v","updated_by":"agent-A"}`))
	}))
	defer ts.Close()

	e, err := New(ts.URL).CtxSet(context.Background(), "global.k", "v", "agent-A")
	if err != nil {
		t.Fatalf("CtxSet: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/context/global.k" {
		t.Fatalf("got %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"value":"v"`) || !strings.Contains(gotBody, `"by":"agent-A"`) {
		t.Fatalf("body=%s", gotBody)
	}
	if e.UpdatedBy != "agent-A" {
		t.Fatalf("entry=%+v", e)
	}
}

func TestCtxList(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("prefix") != "pipeline." {
			t.Errorf("prefix not forwarded: %q", r.URL.Query().Get("prefix"))
		}
		w.Write([]byte(`{"entries":[{"key":"pipeline.p.a","value":"A"}]}`))
	}))
	defer ts.Close()

	got, err := New(ts.URL).CtxList(context.Background(), "pipeline.")
	if err != nil {
		t.Fatalf("CtxList: %v", err)
	}
	if len(got) != 1 || got[0].Key != "pipeline.p.a" {
		t.Fatalf("got %+v", got)
	}
}
```

Ensure `client_test.go`'s import block includes `"io"`, `"net/http"`, `"net/http/httptest"`, `"strings"`, and `"context"` (most are already present from existing tests; add any that are missing).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/client/ -run TestCtx`
Expected: FAIL — `c.CtxSet undefined` / `c.CtxList undefined`.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/client/client.go` (the `net/url` and `time` packages are already imported):

```go
// ContextEntry mirrors the daemon's shared-context entry (GET/PUT /context).
type ContextEntry struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CtxSet writes value at key, attributing the write to `by`.
func (c *Client) CtxSet(ctx context.Context, key, value, by string) (ContextEntry, error) {
	var e ContextEntry
	body := map[string]string{"value": value, "by": by}
	err := c.do(ctx, http.MethodPut, "/context/"+url.PathEscape(key), body, &e)
	return e, err
}

// CtxGet reads the entry at key (StatusError 404 if absent).
func (c *Client) CtxGet(ctx context.Context, key string) (ContextEntry, error) {
	var e ContextEntry
	err := c.do(ctx, http.MethodGet, "/context/"+url.PathEscape(key), nil, &e)
	return e, err
}

// CtxList lists entries under prefix (empty = all).
func (c *Client) CtxList(ctx context.Context, prefix string) ([]ContextEntry, error) {
	p := "/context"
	if prefix != "" {
		p += "?prefix=" + url.QueryEscape(prefix)
	}
	var resp struct {
		Entries []ContextEntry `json:"entries"`
	}
	if err := c.do(ctx, http.MethodGet, p, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Entries, nil
}

// CtxDel deletes key.
func (c *Client) CtxDel(ctx context.Context, key string) error {
	return c.do(ctx, http.MethodDelete, "/context/"+url.PathEscape(key), nil, nil)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/client/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/client/client.go internal/client/client_test.go
git commit -m "feat(client): CtxSet/CtxGet/CtxList/CtxDel"
```

---

## Task 6: CLI `ctx` command group

**Files:**
- Create: `internal/cli/context.go`
- Test: `internal/cli/context_test.go`
- Modify: `internal/cli/root.go` (register the command)

- [ ] **Step 1: Write the failing test**

Create `internal/cli/context_test.go`:

```go
package cli

import (
	"strings"
	"testing"
)

func TestResolveCtxValueFromArg(t *testing.T) {
	v, err := resolveCtxValue("", false, nil, []string{"key", "the-value"})
	if err != nil || v != "the-value" {
		t.Fatalf("got %q err=%v", v, err)
	}
}

func TestResolveCtxValueFromStdin(t *testing.T) {
	v, err := resolveCtxValue("", true, strings.NewReader("piped"), []string{"key"})
	if err != nil || v != "piped" {
		t.Fatalf("got %q err=%v", v, err)
	}
}

func TestResolveCtxValueMissingErrors(t *testing.T) {
	if _, err := resolveCtxValue("", false, nil, []string{"key"}); err == nil {
		t.Fatalf("expected error when no value/file/stdin")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestResolveCtxValue`
Expected: FAIL — `undefined: resolveCtxValue`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/cli/context.go`:

```go
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// ctxWriter is the default writer identity: the agent's own session id when set
// (pipeline/agent context), otherwise "human".
func ctxWriter() string {
	if id := os.Getenv("AGENTCTL_SESSION_ID"); id != "" {
		return id
	}
	return "human"
}

// resolveCtxValue picks the value source: --file, --stdin, or the positional arg.
func resolveCtxValue(fileFlag string, useStdin bool, stdin io.Reader, args []string) (string, error) {
	if fileFlag != "" {
		b, err := os.ReadFile(fileFlag)
		return string(b), err
	}
	if useStdin {
		b, err := io.ReadAll(stdin)
		return string(b), err
	}
	if len(args) < 2 {
		return "", errors.New("provide a value argument, --file, or --stdin")
	}
	return args[1], nil
}

func newCtxCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ctx",
		Short: "Read and write the shared context (a namespaced key/value store agents share)",
	}
	cmd.AddCommand(newCtxSetCmd(), newCtxGetCmd(), newCtxListCmd(), newCtxDelCmd())
	return cmd
}

func newCtxSetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <key> [value]",
		Short: "Set a context key (value inline, or --file / --stdin)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			fileFlag, _ := cmd.Flags().GetString("file")
			useStdin, _ := cmd.Flags().GetBool("stdin")
			value, err := resolveCtxValue(fileFlag, useStdin, cmd.InOrStdin(), args)
			if err != nil {
				return err
			}
			by, _ := cmd.Flags().GetString("as")
			if by == "" {
				by = ctxWriter()
			}
			if _, err := clientFor(cmd).CtxSet(cmd.Context(), args[0], value, by); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "set %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().String("file", "", "read value from a file")
	cmd.Flags().Bool("stdin", false, "read value from stdin")
	cmd.Flags().String("as", "", "writer identity (defaults to $AGENTCTL_SESSION_ID or 'human')")
	return cmd
}

func newCtxGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Print the value at a context key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := clientFor(cmd).CtxGet(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), e.Value)
			return nil
		},
	}
}

func newCtxListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list [prefix]",
		Short: "List context keys (optionally filtered by prefix)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prefix := ""
			if len(args) == 1 {
				prefix = args[0]
			}
			entries, err := clientFor(cmd).CtxList(cmd.Context(), prefix)
			if err != nil {
				return err
			}
			for _, e := range entries {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t(%s, %s)\n", e.Key, e.UpdatedBy, e.UpdatedAt.Format(time.RFC3339))
			}
			return nil
		},
	}
}

func newCtxDelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "del <key>",
		Short: "Delete a context key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := clientFor(cmd).CtxDel(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted %s\n", args[0])
			return nil
		},
	}
}
```

- [ ] **Step 4: Register the command**

In `internal/cli/root.go`, add to the `root.AddCommand(...)` group (e.g. right after the `newSendCmd(), newTailCmd()` line):

```go
	root.AddCommand(newSendCmd(), newTailCmd())
	root.AddCommand(newCtxCmd())
```

- [ ] **Step 5: Run test + build to verify it passes**

Run: `go test ./internal/cli/... && go build ./...`
Expected: PASS, no build errors.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/context.go internal/cli/context_test.go internal/cli/root.go
git commit -m "feat(cli): ctx set/get/list/del command group"
```

---

## Task 7: MCP tools for shared context

**Files:**
- Modify: `internal/mcp/server.go`

**Note on this task's shape:** The existing `internal/mcp/server_test.go` tests (per the grep done while writing this plan) do NOT dispatch tools through the SDK — each stands up an `httptest` daemon stub, builds `NewServer(daemon.URL)`, and asserts the **client method** the tool wraps hits the right daemon path. The tool handlers are thin pass-throughs. We follow that same convention: the guard test exercises the client path, and the tool **registration** is verified by `go build` (an unregistered/typo'd tool or undefined arg struct fails to compile). There is no behavioral red→green here beyond compilation — this is wiring.

- [ ] **Step 1: Write the guard test (matches existing convention)**

Append to `internal/mcp/server_test.go`:

```go
func TestCtxSetClientPath(t *testing.T) {
	var gotPath, gotMethod string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Write([]byte(`{"key":"global.k","value":"v","updated_by":"agent"}`))
	}))
	defer daemon.Close()

	srv := NewServer(daemon.URL)
	if _, err := srv.cl.CtxSet(context.Background(), "global.k", "v", "agent"); err != nil {
		t.Fatalf("CtxSet: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/context/global.k" {
		t.Fatalf("got %s %s", gotMethod, gotPath)
	}
}
```

(`"context"`, `"net/http"`, and `"net/http/httptest"` are already imported in that test file.)

- [ ] **Step 2: Run the test (green once Task 5 is merged)**

Run: `go test ./internal/mcp/ -run TestCtxSetClientPath`
Expected: PASS (this asserts the client path the tool wraps; Task 5 must be complete). If `srv.cl.CtxSet` is undefined, Task 5 is not yet merged — complete it first.

- [ ] **Step 3: Add the arg structs and tools**

In `internal/mcp/server.go`, add the arg structs near the other arg types:

```go
type ctxSetArgs struct {
	Key   string `json:"key" jsonschema:"the context key, e.g. global.findings or pipeline.<id>.<job>.output"`
	Value string `json:"value" jsonschema:"the value to store"`
}
type ctxGetArgs struct {
	Key string `json:"key" jsonschema:"the context key to read"`
}
type ctxListArgs struct {
	Prefix string `json:"prefix,omitempty" jsonschema:"optional key prefix filter (empty = all keys)"`
}
```

Add a small writer-identity helper near `textResult`:

```go
// ctxWriter attributes shared-context writes to this agent when running inside
// one (AGENTCTL_SESSION_ID), else a generic "agent".
func ctxWriter() string {
	if id := os.Getenv("AGENTCTL_SESSION_ID"); id != "" {
		return id
	}
	return "agent"
}
```

Then register the three tools inside `NewServer` (after the existing `get_agent_output` tool block, before `terminate_agent`):

```go
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "ctx_set",
		Description: "Write a value to the shared context — a key/value store all agents share. Use to publish a result other agents will read (e.g. global.findings).",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a ctxSetArgs) (*mcpsdk.CallToolResult, any, error) {
		if _, err := s.cl.CtxSet(ctx, a.Key, a.Value, ctxWriter()); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("set " + a.Key), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "ctx_get",
		Description: "Read a value from the shared context by key.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a ctxGetArgs) (*mcpsdk.CallToolResult, any, error) {
		e, err := s.cl.CtxGet(ctx, a.Key)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult(e.Value), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "ctx_list",
		Description: "List shared-context keys, optionally filtered by prefix.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a ctxListArgs) (*mcpsdk.CallToolResult, any, error) {
		entries, err := s.cl.CtxList(ctx, a.Prefix)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		res, err := jsonResult(entries)
		return res, nil, err
	})
```

(`os` is already imported in `internal/mcp/server.go`.)

- [ ] **Step 4: Run tests + build to verify they pass**

Run: `go test ./internal/mcp/... && go build ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/mcp/server.go internal/mcp/server_test.go
git commit -m "feat(mcp): ctx_set/ctx_get/ctx_list tools"
```

---

## Task 8: Document the `ctx` commands

**Files:**
- Modify: `docs/USAGE.md`

- [ ] **Step 1: Add a shared-context section**

Append a section to `docs/USAGE.md` (match the document's existing heading style — read the file first and mirror it):

```markdown
## Shared context

A namespaced key/value store the daemon owns, so agents can share results.

    agentctl ctx set global.findings "auth.py needs refactor"   # inline value
    agentctl ctx set report.body --file ./report.md             # value from a file
    some-command | agentctl ctx set logs.tail --stdin           # value from stdin
    agentctl ctx get global.findings                            # prints the value
    agentctl ctx list pipeline.                                 # keys under a prefix
    agentctl ctx del global.findings

Writes are attributed to `$AGENTCTL_SESSION_ID` when set (so a spawned agent's
writes are tagged with its id), otherwise to `human`. Override with `--as`.
Keys are free-form dot-namespaced strings (`global.*`, `pipeline.<id>.*`,
`agent.<sid>.*`). Also available as MCP tools `ctx_set` / `ctx_get` / `ctx_list`.
```

- [ ] **Step 2: Verify the full suite once more**

Run: `go build ./... && go test ./...`
Expected: PASS across all packages.

- [ ] **Step 3: Commit**

```bash
git add docs/USAGE.md
git commit -m "docs: document agentctl ctx shared-context commands"
```

---

## Verification checklist (after all tasks)

- [ ] `go build ./...` clean.
- [ ] `go test ./...` green.
- [ ] `make lint` clean (run it; fix any findings).
- [ ] Manual smoke (requires a rebuilt+restarted daemon — `make release && make install`, then restart the launchd daemon):
  - `agentctl ctx set global.hello world` → `set global.hello`
  - `agentctl ctx get global.hello` → `world`
  - `agentctl ctx list` → shows `global.hello\t(human, <ts>)`
  - `agentctl ctx del global.hello` → `deleted global.hello`; subsequent `get` exits non-zero with "context key not found".
```
