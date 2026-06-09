# agentctl GUI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A local web dashboard (Astro + React) served by the existing `agentctl daemon` that lists agents, creates them, manages them (send input, read output), shows full per-agent history, live-updates over SSE, shows busy/idle state, and terminates agents (including worktree/branch).

**Architecture:** The Astro app builds to static files embedded into the Go binary via `go:embed` and served at `/` on `:8765` (same origin as the REST API — no CORS). Live updates are pushed via a new `GET /events/stream` SSE endpoint, fed by an in-process broadcaster (`hub`) that every mutating handler and the poller notify. The browser uses `EventSource` for the session list and short-polls `GET /sessions/{id}/output` for the open agent's live terminal.

**Tech Stack:** Go 1.26 (chi, `go:embed`, `net/http` SSE), Astro 5 + React 19 (`@astrojs/react`), Vitest (frontend unit tests), Node 25 / npm 11.

**Reference spec:** `docs/superpowers/specs/2026-06-01-agentctl-gui-design.md`

---

## Conventions & ground rules

- Module path: `github.com/srajanpathak/agentctl`. Work on a feature branch (the executor sets up an isolated worktree first).
- The daemon REST API is unchanged except for the two new GET routes (`/events/stream`, `/*` static). All actions reuse existing endpoints.
- **Go parts: strict TDD** (write failing test → watch fail → implement → watch pass → commit). **Frontend pure logic** (`status.ts`, `api.ts`): TDD with Vitest. **React components**: build-and-verify (no unit test required; covered by a manual/Playwright smoke at the end).
- Commit after each task with the given message (no Co-Authored-By footer for these personal-project commits).
- Run Go tests with `go test ./...`; frontend tests with `cd web && npm test`.

## File structure

```
agentctl/
├── Makefile                         # + ui / ui-dev / release / web-test targets
├── .gitignore                       # + web/node_modules, web/dist/* (keep .gitkeep)
├── web/                             # Astro project + Go embed (package webui)
│   ├── embed.go                     # //go:embed all:dist → Dist() fs.FS
│   ├── package.json  astro.config.mjs  tsconfig.json  vitest.config.ts
│   ├── dist/.gitkeep                # committed so go:embed always compiles
│   ├── public/                      # static passthrough (favicon)
│   └── src/
│       ├── pages/index.astro        # shell mounting <Dashboard client:load />
│       ├── styles/app.css           # compact dashboard styling
│       ├── lib/
│       │   ├── types.ts             # Session / AgentEvent / Status (mirror Go JSON)
│       │   ├── status.ts            # busyIdle(status) → Badge  (+ status.test.ts)
│       │   └── api.ts               # fetch helpers + subscribeSessions (+ api.test.ts)
│       └── components/
│           ├── Dashboard.tsx        # root island: SSE + selection + create modal
│           ├── AgentList.tsx        # live table + BusyIdleBadge + age/detail
│           ├── BusyIdleBadge.tsx    # status → colored badge
│           ├── EventTimeline.tsx    # events[] newest-first
│           ├── AgentDetail.tsx      # metadata + live output poll + send box + history
│           ├── NewAgentModal.tsx    # type-aware create form → spawn
│           └── TerminateControls.tsx# cleanup with 409 guard → force/hard
└── internal/daemon/
    ├── hub.go                       # broadcaster (+ hub_test.go)
    ├── sse.go                       # GET /events/stream (+ sse_test.go)
    ├── static.go                    # /* static handler (+ static_test.go)
    ├── api.go                       # router(): + /events/stream, + registerStatic; + notify()
    └── server.go                    # NewServer: create hub, wire poller.OnChange
```

Phase order (each phase builds and tests green on its own): **A** daemon static embed → **B** hub → **C** SSE + notify wiring → **D** Astro scaffold (end-to-end embed) → **E** frontend lib (TDD) → **F** list/badge/timeline/Dashboard → **G** detail/create/terminate → **H** integration + README.

---

## Phase A — Daemon: static embed + serving

### Task A1: Embed package with a build-safe placeholder

**Files:**
- Create: `web/dist/.gitkeep`
- Create: `web/embed.go`
- Modify: `.gitignore`

- [ ] **Step 1: Create the placeholder and gitignore rules**

Create an empty file `web/dist/.gitkeep` (so the embed dir always has content).

Append to `.gitignore` (create it if missing):
```gitignore
# agentctl web UI
web/node_modules/
web/dist/*
!web/dist/.gitkeep
```

- [ ] **Step 2: Write the embed package**

Create `web/embed.go`:
```go
// Package webui embeds the built Astro dashboard so the daemon can serve it.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var files embed.FS

// Dist returns the built UI filesystem rooted at the dist/ directory.
// Before `make ui` runs it contains only a placeholder; the daemon then
// serves an inline "UI not built" page (see internal/daemon/static.go).
func Dist() fs.FS {
	sub, err := fs.Sub(files, "dist")
	if err != nil {
		panic(err) // dist is always embedded; this is a programmer error
	}
	return sub
}
```

- [ ] **Step 3: Verify it compiles**

Run: `go build ./web/`
Expected: success (the `all:dist` embed picks up `.gitkeep`).

- [ ] **Step 4: Commit**

```bash
git add web/embed.go web/dist/.gitkeep .gitignore
git commit -m "feat: webui embed package with build-safe placeholder"
```

### Task A2: Static file handler + route wiring

**Files:**
- Create: `internal/daemon/static.go`
- Create: `internal/daemon/static_test.go`
- Modify: `internal/daemon/api.go` (router)

- [ ] **Step 1: Write the failing test**

Create `internal/daemon/static_test.go`:
```go
package daemon

import (
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStaticServesIndexAtRoot(t *testing.T) {
	ts := testServer(t, newFakeStore())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	body, _ := io.ReadAll(resp.Body)
	require.Contains(t, string(body), "agentctl")
}

func TestStaticDoesNotShadowAPI(t *testing.T) {
	ts := testServer(t, newFakeStore())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/sessions")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "application/json")
}

func TestStaticSPAFallback(t *testing.T) {
	ts := testServer(t, newFakeStore())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/some/client/route")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/html")
}
```

(`testServer` and `newFakeStore` already exist in `internal/daemon/api_test.go`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestStatic`
Expected: FAIL — routes return 404 (no static handler yet) / `registerStatic` undefined once referenced.

- [ ] **Step 3: Implement the static handler**

Create `internal/daemon/static.go`:
```go
package daemon

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
	webui "github.com/srajanpathak/agentctl/web"
)

const fallbackHTML = `<!doctype html><html><head><meta charset="utf-8"><title>agentctl</title></head>` +
	`<body><h1>agentctl</h1><p>UI not built. Run <code>make ui</code> (or <code>make release</code>) and restart the daemon.</p></body></html>`

// registerStatic serves the embedded Astro UI for any non-API GET path.
// It MUST be registered last so chi's explicit API routes take precedence.
func (s *Server) registerStatic(r chi.Router) {
	ui := webui.Dist()
	fileServer := http.FileServerFS(ui)
	r.Get("/*", func(w http.ResponseWriter, req *http.Request) {
		p := strings.TrimPrefix(path.Clean(req.URL.Path), "/")
		if p == "" || p == "." {
			serveIndex(w, ui)
			return
		}
		if st, err := fs.Stat(ui, p); err != nil || st.IsDir() {
			serveIndex(w, ui) // SPA fallback for unknown client routes
			return
		}
		fileServer.ServeHTTP(w, req)
	})
}

func serveIndex(w http.ResponseWriter, ui fs.FS) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if b, err := fs.ReadFile(ui, "index.html"); err == nil {
		_, _ = w.Write(b)
		return
	}
	_, _ = w.Write([]byte(fallbackHTML))
}
```

- [ ] **Step 4: Wire it into the router (last)**

In `internal/daemon/api.go`, update `router()`:
```go
func (s *Server) router() http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", s.handleHealthz)
	r.Get("/sessions", s.handleListSessions)
	r.Get("/sessions/{id}", s.handleGetSession)
	r.Post("/events", s.handleEvent)
	s.registerLifecycleRoutes(r)
	s.registerStatic(r) // catch-all; must be last
	return r
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/daemon/`
Expected: PASS (all daemon tests, including the 3 new static tests — root + fallback serve the inline `fallbackHTML` since dist has only `.gitkeep`).

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/static.go internal/daemon/static_test.go internal/daemon/api.go
git commit -m "feat: daemon serves embedded UI with SPA fallback (API routes take precedence)"
```

### Task A3: Makefile targets for the UI

**Files:**
- Modify: `Makefile`

- [ ] **Step 1: Add UI build/dev/test targets**

Replace the `.PHONY` line and append targets in `Makefile`:
```make
.PHONY: build test lint mongo-up mongo-down run-daemon ui ui-dev web-test release

ui:
	cd web && npm ci && npm run build

ui-dev:
	cd web && npm run dev

web-test:
	cd web && npm test

# Full release build: build the UI first so go:embed picks up real assets.
release: ui build
```

(Leave the existing `build`, `test`, `lint`, `mongo-up`, `mongo-down`, `run-daemon` targets as-is. `make build` stays Go-only and embeds whatever is in `web/dist`; `make release` rebuilds the UI first.)

- [ ] **Step 2: Verify make parses**

Run: `make -n release`
Expected: prints the `npm ci && npm run build` then `go build` commands without executing meaningfully (dry run).

- [ ] **Step 3: Commit**

```bash
git add Makefile
git commit -m "chore: makefile ui/ui-dev/web-test/release targets"
```

---

## Phase B — Daemon: broadcaster (hub)

### Task B1: The hub

**Files:**
- Create: `internal/daemon/hub.go`
- Create: `internal/daemon/hub_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/daemon/hub_test.go`:
```go
package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHubPublishWakesSubscriber(t *testing.T) {
	h := newHub()
	ch, unsub := h.subscribe()
	defer unsub()
	h.publish()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("subscriber was not notified")
	}
}

func TestHubCoalesces(t *testing.T) {
	h := newHub()
	ch, unsub := h.subscribe()
	defer unsub()
	h.publish()
	h.publish()
	h.publish()
	// cap-1 channel collapses bursts into a single pending notification.
	<-ch
	select {
	case <-ch:
		t.Fatal("expected coalesced single notification")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHubUnsubscribeStopsDelivery(t *testing.T) {
	h := newHub()
	ch, unsub := h.subscribe()
	unsub()
	h.publish()
	select {
	case _, ok := <-ch:
		require.False(t, ok, "channel should be closed after unsubscribe")
	case <-time.After(50 * time.Millisecond):
		t.Fatal("expected closed channel")
	}
}

func TestHubConcurrentPublishIsRaceClean(t *testing.T) {
	h := newHub()
	ch, unsub := h.subscribe()
	defer unsub()
	go func() { for i := 0; i < 100; i++ { <-ch } }()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h.publish() }()
	}
	wg.Wait()
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestHub`
Expected: FAIL — `undefined: newHub`.

- [ ] **Step 3: Implement the hub**

Create `internal/daemon/hub.go`:
```go
package daemon

import "sync"

// hub is a tiny in-process broadcaster. Subscribers get a coalescing (cap-1)
// channel: publish() never blocks, and bursts collapse into one pending tick.
type hub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func newHub() *hub {
	return &hub{subs: make(map[chan struct{}]struct{})}
}

func (h *hub) subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	unsub := func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			close(ch)
			h.mu.Unlock()
		})
	}
	return ch, unsub
}

func (h *hub) publish() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default: // already has a pending tick — coalesce
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/daemon/ -run TestHub`
Expected: PASS (all four, race-clean).

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/hub.go internal/daemon/hub_test.go
git commit -m "feat: in-process broadcaster (hub) for SSE fan-out"
```

---

## Phase C — Daemon: SSE endpoint + change notifications

### Task C1: Server owns a hub + notify() helper

**Files:**
- Modify: `internal/daemon/api.go` (Server struct + notify)
- Modify: `internal/daemon/server.go` (NewServer creates hub, wires poller)
- Modify: `internal/poller/poller.go` (OnChange callback)
- Modify: `internal/poller/poller_test.go` (OnChange test)

- [ ] **Step 1: Write the failing poller test**

Append to `internal/poller/poller_test.go`:
```go
func TestTickCallsOnChangeWhenStatusChanges(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{{ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking}},
		alive:    map[string]bool{"A-1": false}, // → orphaned (a change)
		updates:  map[string]store.Status{},
	}
	p := New(d, 5*time.Minute)
	called := 0
	p.OnChange = func() { called++ }
	require.NoError(t, p.tick(context.Background()))
	require.Equal(t, 1, called, "OnChange fires once when a status changed")
}

func TestTickNoOnChangeWhenNothingChanges(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{{
			ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking,
			UpdatedAt: time.Now(), LastPaneExcerpt: "x",
		}},
		alive:   map[string]bool{"A-1": true},
		panes:   map[string]string{"A-1": "x"}, // unchanged pane, fresh → no change
		updates: map[string]store.Status{},
	}
	p := New(d, 5*time.Minute)
	called := 0
	p.OnChange = func() { called++ }
	require.NoError(t, p.tick(context.Background()))
	require.Equal(t, 0, called)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/poller/ -run TestTickCallsOnChange`
Expected: FAIL — `p.OnChange` undefined.

- [ ] **Step 3: Add OnChange to the poller**

In `internal/poller/poller.go`, add the field to the struct:
```go
type Poller struct {
	deps       Deps
	stuckAfter time.Duration
	// OnChange, if set, is called once after a tick that changed any session
	// (status or pane). The daemon wires this to hub.publish for SSE.
	OnChange func()
}
```

Replace the body of `tick` to track changes and fire `OnChange`:
```go
func (p *Poller) tick(ctx context.Context) error {
	sessions, err := p.deps.List(ctx)
	if err != nil {
		return err
	}
	changed := false
	for _, s := range sessions {
		if isTerminal(s.Status) {
			continue
		}
		alive := p.deps.SessionAlive(ctx, s.TmuxSession)
		var pane string
		if alive {
			pane, _ = p.deps.CapturePane(ctx, s.TmuxSession)
			if excerpt := lastLines(pane, 20); excerpt != s.LastPaneExcerpt {
				_ = p.deps.UpdatePane(ctx, s.ID, excerpt)
				changed = true
			}
		}
		next := classify(s, pane, alive, time.Since(s.UpdatedAt), p.stuckAfter)
		if next != s.Status {
			if err := p.deps.UpdateStatus(ctx, s.ID, next); err != nil {
				log.Printf("poller: update %s: %v", s.ID, err)
			} else {
				changed = true
			}
		}
	}
	if changed && p.OnChange != nil {
		p.OnChange()
	}
	return nil
}
```

- [ ] **Step 4: Run poller tests**

Run: `go test ./internal/poller/`
Expected: PASS (all, including the two new OnChange tests).

- [ ] **Step 5: Add the hub to Server + notify() helper**

In `internal/daemon/api.go`, add a `hub` field to the `Server` struct:
```go
type Server struct {
	store        store.Store
	life         Lifecycle
	poller       *poller.Poller
	pollInterval time.Duration
	hub          *hub
}
```

Add the helper (anywhere in `api.go`):
```go
// notify signals SSE subscribers that session state changed. Safe with a nil
// hub (some tests construct Server literals without one).
func (s *Server) notify() {
	if s.hub != nil {
		s.hub.publish()
	}
}
```

- [ ] **Step 6: Create the hub in NewServer and wire the poller**

In `internal/daemon/server.go`, update `NewServer`:
```go
func NewServer(st store.Store, life Lifecycle, p *poller.Poller, interval time.Duration) *Server {
	h := newHub()
	if p != nil {
		p.OnChange = h.publish
	}
	return &Server{store: st, life: life, poller: p, pollInterval: interval, hub: h}
}
```

- [ ] **Step 7: Verify build + tests**

Run: `go build ./... && go test ./internal/poller/ ./internal/daemon/`
Expected: PASS / clean build. (`cli/daemon.go` is unchanged — `NewServer` keeps its signature; the poller wiring happens inside it.)

- [ ] **Step 8: Commit**

```bash
git add internal/poller/poller.go internal/poller/poller_test.go internal/daemon/api.go internal/daemon/server.go
git commit -m "feat: poller OnChange callback + Server hub wiring"
```

### Task C2: SSE endpoint `GET /events/stream`

**Files:**
- Create: `internal/daemon/sse.go`
- Create: `internal/daemon/sse_test.go`
- Modify: `internal/daemon/api.go` (router registers the route)

- [ ] **Step 1: Write the failing test**

Create `internal/daemon/sse_test.go`:
```go
package daemon

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

func sseServer(t *testing.T, fs *fakeStore) *Server {
	t.Helper()
	return &Server{store: fs, hub: newHub()}
}

// readEvent reads lines until a blank line, returning the joined "data:" payload.
func readEvent(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	var data []string
	for {
		line, err := r.ReadString('\n')
		require.NoError(t, err)
		line = strings.TrimRight(line, "\n")
		if line == "" {
			if len(data) > 0 {
				return strings.Join(data, "\n")
			}
			continue // heartbeat / comment-only block
		}
		if strings.HasPrefix(line, "data: ") {
			data = append(data, strings.TrimPrefix(line, "data: "))
		}
	}
}

func TestSSEInitialSnapshotThenPush(t *testing.T) {
	fs := newFakeStore()
	fs.data["A-1"] = &store.Session{ID: "A-1", Status: store.StatusWorking}
	srv := sseServer(t, fs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/events/stream", nil)
	rec := newStreamRecorder()
	go srv.handleEventsStream(rec, req)

	r := bufio.NewReader(rec.reader())
	first := readEvent(t, r)
	require.Contains(t, first, `"A-1"`)

	// A new session + publish → second snapshot.
	fs.data["B-2"] = &store.Session{ID: "B-2", Status: store.StatusIdle}
	srv.hub.publish()
	second := readEvent(t, r)
	require.Contains(t, second, `"B-2"`)
}
```

Also add a tiny streaming recorder helper to `sse_test.go` (httptest.ResponseRecorder doesn't stream):
```go
import (
	"io"
	"net/http"
	"sync"
)

// streamRecorder is a flushable, streaming ResponseWriter backed by an io.Pipe.
type streamRecorder struct {
	hdr  http.Header
	pw   *io.PipeWriter
	pr   *io.PipeReader
	once sync.Once
	code int
}

func newStreamRecorder() *streamRecorder {
	pr, pw := io.Pipe()
	return &streamRecorder{hdr: make(http.Header), pr: pr, pw: pw}
}
func (s *streamRecorder) Header() http.Header { return s.hdr }
func (s *streamRecorder) WriteHeader(code int) { s.code = code }
func (s *streamRecorder) Write(b []byte) (int, error) { return s.pw.Write(b) }
func (s *streamRecorder) Flush() {}
func (s *streamRecorder) reader() io.Reader { return s.pr }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestSSE`
Expected: FAIL — `srv.handleEventsStream` undefined.

- [ ] **Step 3: Implement the SSE handler**

Create `internal/daemon/sse.go`:
```go
package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/srajanpathak/agentctl/internal/store"
)

// handleEventsStream streams the full session list as SSE. It sends an initial
// snapshot, then a new one whenever the hub fires (deduped), plus a heartbeat.
func (s *Server) handleEventsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, unsub := s.hub.subscribe()
	defer unsub()

	var last []byte
	send := func() bool {
		sessions, err := s.store.List(r.Context())
		if err != nil {
			return true // transient; try again on next signal
		}
		if sessions == nil {
			sessions = []*store.Session{}
		}
		payload, err := json.Marshal(sessionsResponse{Sessions: sessions})
		if err != nil {
			return true
		}
		if bytes.Equal(payload, last) {
			return true // nothing changed since last send
		}
		last = payload
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !send() {
		return
	}

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if !send() {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
```

- [ ] **Step 4: Register the route**

In `internal/daemon/api.go` `router()`, add the SSE route before `registerStatic`:
```go
	r.Post("/events", s.handleEvent)
	r.Get("/events/stream", s.handleEventsStream)
	s.registerLifecycleRoutes(r)
	s.registerStatic(r)
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/daemon/ -run TestSSE`
Expected: PASS (initial snapshot contains A-1; after publish, second contains B-2).

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/sse.go internal/daemon/sse_test.go internal/daemon/api.go
git commit -m "feat: GET /events/stream SSE endpoint (snapshot push + heartbeat)"
```

### Task C3: Notify on every mutation

**Files:**
- Modify: `internal/daemon/api.go` (handleEvent)
- Modify: `internal/daemon/lifecycle_routes.go` (handleSpawn/handleCleanup/handleInput)
- Modify: `internal/daemon/lifecycle_routes_test.go` (assert notify fires)

- [ ] **Step 1: Write the failing test**

Append to `internal/daemon/lifecycle_routes_test.go`:
```go
func TestSpawnNotifiesSubscribers(t *testing.T) {
	fl := &fakeLife{}
	srv := &Server{store: newFakeStore(), life: fl, hub: newHub()}
	ch, unsub := srv.hub.subscribe()
	defer unsub()

	ts := httptest.NewServer(srv.router())
	defer ts.Close()
	body, _ := json.Marshal(SpawnRequest{Type: "development", Ticket: "A-1", Repo: "/repo"})
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("spawn did not notify SSE subscribers")
	}
}
```

Ensure `sseServer`-style `time` import exists in this test file (add `"time"` to its import block if missing).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestSpawnNotifies`
Expected: FAIL — no notification (handlers don't call `notify` yet).

- [ ] **Step 3: Add notify() calls after successful mutations**

In `internal/daemon/lifecycle_routes.go`:
- In `handleSpawn`, after `writeJSON(w, http.StatusCreated, sess)` succeeds, add `s.notify()` immediately before the final `writeJSON` is fine too — place it right after the successful `s.store.Insert(...)` check:
```go
	if err := s.store.Insert(r.Context(), sess); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notify()
	writeJSON(w, http.StatusCreated, sess)
```
- In `handleCleanup`, after the archive/delete branch, before the final `writeJSON`:
```go
	if req.Hard {
		_ = s.store.Delete(r.Context(), req.ID)
	} else {
		_ = s.store.Archive(r.Context(), req.ID)
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleaned"})
```
- In `handleInput`, after a successful `s.life.Input(...)`:
```go
	if err := s.life.Input(r.Context(), sess.TmuxSession, req.Text); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
```

In `internal/daemon/api.go` `handleEvent`, after the status update path completes (just before the final `writeJSON`):
```go
	if st := statusForHook(req.Type); st != "" {
		if err := s.store.UpdateStatus(ctx, req.Session, st); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/`
Expected: PASS (all daemon tests, including `TestSpawnNotifiesSubscribers`).

- [ ] **Step 5: Full Go suite + race**

Run: `go test ./... && go test -race ./internal/daemon/ ./internal/poller/`
Expected: PASS / race-clean.

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/lifecycle_routes.go internal/daemon/api.go internal/daemon/lifecycle_routes_test.go
git commit -m "feat: notify SSE subscribers on spawn/cleanup/input/event mutations"
```

---

## Phase D — Astro scaffold (end-to-end embed)

### Task D1: Astro project config

**Files:**
- Create: `web/package.json`, `web/astro.config.mjs`, `web/tsconfig.json`, `web/vitest.config.ts`

- [ ] **Step 1: package.json**

Create `web/package.json`:
```json
{
  "name": "agentctl-web",
  "private": true,
  "type": "module",
  "scripts": {
    "dev": "astro dev",
    "build": "astro build",
    "preview": "astro preview",
    "test": "vitest run"
  },
  "dependencies": {
    "@astrojs/react": "^4.2.0",
    "astro": "^5.4.0",
    "react": "^19.0.0",
    "react-dom": "^19.0.0"
  },
  "devDependencies": {
    "@types/react": "^19.0.0",
    "@types/react-dom": "^19.0.0",
    "jsdom": "^26.0.0",
    "typescript": "^5.7.0",
    "vitest": "^3.0.0"
  }
}
```

- [ ] **Step 2: astro.config.mjs (static output + dev proxy)**

Create `web/astro.config.mjs`:
```js
import { defineConfig } from 'astro/config';
import react from '@astrojs/react';

const DAEMON = 'http://127.0.0.1:8765';
const proxy = (path) => ({ [path]: { target: DAEMON, changeOrigin: true } });

// In dev, forward API + SSE to the running daemon so the browser stays
// same-origin (matches prod where the daemon serves this build).
export default defineConfig({
  output: 'static',
  outDir: './dist',
  integrations: [react()],
  server: { port: 4321 },
  vite: {
    server: {
      proxy: {
        ...proxy('/sessions'),
        ...proxy('/spawn'),
        ...proxy('/cleanup'),
        ...proxy('/events'),
        ...proxy('/healthz'),
      },
    },
  },
});
```

- [ ] **Step 3: tsconfig.json**

Create `web/tsconfig.json`:
```json
{
  "extends": "astro/tsconfigs/strict",
  "compilerOptions": {
    "jsx": "react-jsx",
    "jsxImportSource": "react"
  }
}
```

- [ ] **Step 4: vitest.config.ts**

Create `web/vitest.config.ts`:
```ts
import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    environment: 'jsdom',
    globals: true,
  },
});
```

- [ ] **Step 5: Install deps**

Run: `cd web && npm install`
Expected: resolves and writes `package-lock.json` + `node_modules/` (gitignored).

- [ ] **Step 6: Commit**

```bash
git add web/package.json web/astro.config.mjs web/tsconfig.json web/vitest.config.ts web/package-lock.json
git commit -m "chore: scaffold astro+react project config with dev proxy"
```

### Task D2: Minimal shell page + verify embed end-to-end

**Files:**
- Create: `web/src/pages/index.astro`
- Create: `web/src/components/Dashboard.tsx` (placeholder, replaced in Phase F)
- Create: `web/src/styles/app.css` (minimal; expanded later)

- [ ] **Step 1: Placeholder Dashboard island**

Create `web/src/components/Dashboard.tsx`:
```tsx
export default function Dashboard() {
  return <main><h1>agentctl</h1><p>Dashboard loading…</p></main>;
}
```

- [ ] **Step 2: Shell page**

Create `web/src/pages/index.astro`:
```astro
---
import Dashboard from '../components/Dashboard.tsx';
import '../styles/app.css';
---
<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>agentctl</title>
  </head>
  <body>
    <Dashboard client:load />
  </body>
</html>
```

- [ ] **Step 3: Minimal stylesheet**

Create `web/src/styles/app.css`:
```css
:root { font-family: ui-sans-serif, system-ui, sans-serif; color-scheme: light dark; }
body { margin: 0; }
```

- [ ] **Step 4: Build the UI and verify embed serving**

Run:
```bash
cd web && npm run build && cd ..
go build -o bin/agentctl ./cmd/agentctl
make mongo-up
./bin/agentctl daemon & sleep 1
curl -s localhost:8765/ | head -c 200      # should be the built index.html (contains agentctl + a script tag)
curl -s localhost:8765/sessions            # still {"sessions":[]}
kill %1
```
Expected: `/` returns the real built HTML (not the inline fallback — it includes a `<script>` from `/_astro/`), `/sessions` still JSON.

- [ ] **Step 5: Commit (source only; dist stays gitignored)**

```bash
git add web/src
git commit -m "feat: astro shell page + placeholder dashboard island"
```

---

## Phase E — Frontend lib (TDD with Vitest)

### Task E1: Types

**Files:**
- Create: `web/src/lib/types.ts`

- [ ] **Step 1: Define types mirroring the Go JSON**

Create `web/src/lib/types.ts`:
```ts
export type Status =
  | 'spawning' | 'working' | 'waiting_for_input'
  | 'idle' | 'done' | 'errored' | 'orphaned';

export interface AgentEvent {
  ts: string;
  type: string;
  detail: string;
}

export interface Session {
  id: string;
  type: string;
  ticket: string;
  tmux_session: string;
  repo: string;
  worktree: string;
  branch: string;
  pr: string;
  status: Status;
  pid: number;
  created_at: string;
  updated_at: string;
  events: AgentEvent[] | null;
  last_pane_excerpt: string;
}
```

- [ ] **Step 2: Typecheck**

Run: `cd web && npx tsc --noEmit`
Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add web/src/lib/types.ts
git commit -m "feat(web): session/event/status types"
```

### Task E2: Busy/idle mapping (TDD)

**Files:**
- Create: `web/src/lib/status.ts`
- Create: `web/src/lib/status.test.ts`

- [ ] **Step 1: Write the failing test**

Create `web/src/lib/status.test.ts`:
```ts
import { describe, it, expect } from 'vitest';
import { busyIdle } from './status';

describe('busyIdle', () => {
  it('maps working/spawning to busy', () => {
    expect(busyIdle('working')).toEqual({ label: 'Busy', kind: 'busy' });
    expect(busyIdle('spawning')).toEqual({ label: 'Starting', kind: 'busy' });
  });
  it('maps waiting_for_input to attention', () => {
    expect(busyIdle('waiting_for_input')).toEqual({ label: 'Needs input', kind: 'attention' });
  });
  it('maps idle/done to idle', () => {
    expect(busyIdle('idle')).toEqual({ label: 'Idle', kind: 'idle' });
    expect(busyIdle('done')).toEqual({ label: 'Done', kind: 'idle' });
  });
  it('maps errored/orphaned to error', () => {
    expect(busyIdle('errored')).toEqual({ label: 'Error', kind: 'error' });
    expect(busyIdle('orphaned')).toEqual({ label: 'Orphaned', kind: 'error' });
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && npm test`
Expected: FAIL — `./status` has no `busyIdle`.

- [ ] **Step 3: Implement**

Create `web/src/lib/status.ts`:
```ts
import type { Status } from './types';

export type BadgeKind = 'busy' | 'attention' | 'idle' | 'error';
export interface Badge { label: string; kind: BadgeKind; }

export function busyIdle(status: Status): Badge {
  switch (status) {
    case 'spawning': return { label: 'Starting', kind: 'busy' };
    case 'working': return { label: 'Busy', kind: 'busy' };
    case 'waiting_for_input': return { label: 'Needs input', kind: 'attention' };
    case 'idle': return { label: 'Idle', kind: 'idle' };
    case 'done': return { label: 'Done', kind: 'idle' };
    case 'errored': return { label: 'Error', kind: 'error' };
    case 'orphaned': return { label: 'Orphaned', kind: 'error' };
    default: return { label: status, kind: 'idle' };
  }
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd web && npm test`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/status.ts web/src/lib/status.test.ts
git commit -m "feat(web): busy/idle status mapping (tested)"
```

### Task E3: API client (TDD)

**Files:**
- Create: `web/src/lib/api.ts`
- Create: `web/src/lib/api.test.ts`

- [ ] **Step 1: Write the failing test**

Create `web/src/lib/api.test.ts`:
```ts
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { listSessions, spawn, cleanup, ApiError } from './api';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status, headers: { 'Content-Type': 'application/json' },
  });
}

beforeEach(() => { vi.restoreAllMocks(); });

describe('api', () => {
  it('listSessions GETs /sessions and unwraps the array', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ sessions: [{ id: 'A-1' }] }));
    vi.stubGlobal('fetch', fetchMock);
    const out = await listSessions();
    expect(fetchMock).toHaveBeenCalledWith('/sessions');
    expect(out).toHaveLength(1);
    expect(out[0].id).toBe('A-1');
  });

  it('spawn POSTs the full body to /spawn', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ id: 'A-1' }, 201));
    vi.stubGlobal('fetch', fetchMock);
    await spawn({ type: 'development', repo: '/r', ticket: 'A-1' });
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/spawn');
    expect(opts.method).toBe('POST');
    expect(JSON.parse(opts.body)).toEqual({
      type: 'development', ticket: 'A-1', repo: '/r', branch: '', pr: '', worktree: false,
    });
  });

  it('cleanup POSTs id/force/hard to /cleanup', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ status: 'cleaned' }));
    vi.stubGlobal('fetch', fetchMock);
    await cleanup('A-1', true, false);
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/cleanup');
    expect(JSON.parse(opts.body)).toEqual({ id: 'A-1', force: true, hard: false });
  });

  it('throws ApiError with the server error message on non-2xx', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ error: 'already exists' }, 409));
    vi.stubGlobal('fetch', fetchMock);
    await expect(spawn({ type: 'development', repo: '/r' })).rejects.toMatchObject({
      status: 409, message: 'already exists',
    });
    await expect(spawn({ type: 'development', repo: '/r' })).rejects.toBeInstanceOf(ApiError);
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && npm test`
Expected: FAIL — `./api` has no exports.

- [ ] **Step 3: Implement the client**

Create `web/src/lib/api.ts`:
```ts
import type { Session } from './types';

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message);
    this.name = 'ApiError';
  }
}

export interface SpawnParams {
  type: string;
  repo: string;
  ticket?: string;
  branch?: string;
  pr?: string;
  worktree?: boolean;
}

async function parse<T>(res: Response): Promise<T> {
  if (!res.ok) {
    let msg = res.statusText;
    try {
      const body = await res.json();
      if (body && body.error) msg = body.error;
    } catch { /* non-JSON error body */ }
    throw new ApiError(res.status, msg);
  }
  return res.json() as Promise<T>;
}

export async function listSessions(): Promise<Session[]> {
  const data = await parse<{ sessions: Session[] | null }>(await fetch('/sessions'));
  return data.sessions ?? [];
}

export async function getSession(id: string): Promise<Session> {
  return parse<Session>(await fetch(`/sessions/${encodeURIComponent(id)}`));
}

export async function spawn(p: SpawnParams): Promise<Session> {
  return parse<Session>(await fetch('/spawn', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      type: p.type, ticket: p.ticket ?? '', repo: p.repo,
      branch: p.branch ?? '', pr: p.pr ?? '', worktree: !!p.worktree,
    }),
  }));
}

export async function cleanup(id: string, force: boolean, hard: boolean): Promise<void> {
  await parse<unknown>(await fetch('/cleanup', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ id, force, hard }),
  }));
}

export async function sendInput(id: string, text: string): Promise<void> {
  await parse<unknown>(await fetch(`/sessions/${encodeURIComponent(id)}/input`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ text }),
  }));
}

export async function getOutput(id: string, lines = 200): Promise<string> {
  const data = await parse<{ output: string }>(
    await fetch(`/sessions/${encodeURIComponent(id)}/output?lines=${lines}`),
  );
  return data.output;
}

// subscribeSessions opens an SSE connection. Returns an unsubscribe function.
export function subscribeSessions(
  onData: (sessions: Session[]) => void,
  onError: () => void,
  onOpen: () => void,
): () => void {
  const es = new EventSource('/events/stream');
  es.onopen = () => onOpen();
  es.onmessage = (e) => {
    try {
      const d = JSON.parse(e.data) as { sessions: Session[] | null };
      onData(d.sessions ?? []);
    } catch { /* ignore malformed frame */ }
  };
  es.onerror = () => onError();
  return () => es.close();
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd web && npm test`
Expected: PASS (status + api suites).

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/api.ts web/src/lib/api.test.ts
git commit -m "feat(web): daemon API client + SSE subscribe (tested)"
```

---

## Phase F — List, badge, timeline, Dashboard

### Task F1: BusyIdleBadge + EventTimeline + AgentList

**Files:**
- Create: `web/src/components/BusyIdleBadge.tsx`
- Create: `web/src/components/EventTimeline.tsx`
- Create: `web/src/components/AgentList.tsx`

- [ ] **Step 1: BusyIdleBadge**

Create `web/src/components/BusyIdleBadge.tsx`:
```tsx
import type { Status } from '../lib/types';
import { busyIdle } from '../lib/status';

export default function BusyIdleBadge({ status }: { status: Status }) {
  const b = busyIdle(status);
  return <span className={`badge ${b.kind}`} title={status}>{b.label}</span>;
}
```

- [ ] **Step 2: EventTimeline**

Create `web/src/components/EventTimeline.tsx`:
```tsx
import type { AgentEvent } from '../lib/types';

export default function EventTimeline({ events }: { events: AgentEvent[] | null }) {
  const ev = (events ?? []).slice().reverse(); // newest first
  if (ev.length === 0) return <div className="muted">No events yet.</div>;
  return (
    <ul className="timeline">
      {ev.map((e, i) => (
        <li key={i}>
          <time>{e.ts ? new Date(e.ts).toLocaleTimeString() : ''}</time>{' '}
          <b>{e.type}</b>{e.detail && <span className="muted"> — {e.detail}</span>}
        </li>
      ))}
    </ul>
  );
}
```

- [ ] **Step 3: AgentList**

Create `web/src/components/AgentList.tsx`:
```tsx
import type { Session } from '../lib/types';
import BusyIdleBadge from './BusyIdleBadge';

function age(iso: string): string {
  if (!iso) return '—';
  const ms = Date.now() - new Date(iso).getTime();
  const m = Math.floor(ms / 60000);
  if (m < 1) return '<1m';
  if (m < 60) return `${m}m`;
  return `${Math.floor(m / 60)}h${m % 60}m`;
}

function lastDetail(s: Session): string {
  const ev = s.events;
  return ev && ev.length ? ev[ev.length - 1].detail : '';
}

export default function AgentList({ sessions, selectedId, onSelect }: {
  sessions: Session[];
  selectedId: string | null;
  onSelect: (id: string) => void;
}) {
  if (sessions.length === 0) {
    return <div className="list empty">No agents yet. Click “+ New agent”.</div>;
  }
  return (
    <div className="list">
      <table>
        <thead>
          <tr><th>ID</th><th>Type</th><th>State</th><th>Status</th><th>Age</th><th>Detail</th></tr>
        </thead>
        <tbody>
          {sessions.map((s) => (
            <tr key={s.id} className={s.id === selectedId ? 'sel' : ''} onClick={() => onSelect(s.id)}>
              <td>{s.id}</td>
              <td>{s.type}</td>
              <td><BusyIdleBadge status={s.status} /></td>
              <td>{s.status}</td>
              <td>{age(s.updated_at)}</td>
              <td className="muted">{lastDetail(s)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
```

- [ ] **Step 4: Typecheck**

Run: `cd web && npx tsc --noEmit`
Expected: no errors.

- [ ] **Step 5: Commit**

```bash
git add web/src/components/BusyIdleBadge.tsx web/src/components/EventTimeline.tsx web/src/components/AgentList.tsx
git commit -m "feat(web): badge, event timeline, agent list components"
```

### Task F2: Dashboard root island (SSE + selection)

**Files:**
- Modify: `web/src/components/Dashboard.tsx`

> Note: `AgentDetail` and `NewAgentModal` are imported here but created in Phase G. To keep this task building on its own, this version renders a simple inline detail/placeholder; Phase G swaps in the real components. (The imports below are added in Phase G — for now keep the inline versions shown.)

- [ ] **Step 1: Implement the Dashboard with SSE + inline detail placeholder**

Replace `web/src/components/Dashboard.tsx`:
```tsx
import { useEffect, useState } from 'react';
import type { Session } from '../lib/types';
import { listSessions, subscribeSessions } from '../lib/api';
import AgentList from './AgentList';
import BusyIdleBadge from './BusyIdleBadge';

export default function Dashboard() {
  const [sessions, setSessions] = useState<Session[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [connected, setConnected] = useState(false);

  useEffect(() => {
    listSessions().then(setSessions).catch(() => { /* SSE will populate */ });
    const unsub = subscribeSessions(
      setSessions,
      () => setConnected(false),
      () => setConnected(true),
    );
    return unsub;
  }, []);

  const selected = sessions.find((s) => s.id === selectedId) ?? null;

  return (
    <div className="layout">
      <header className="topbar">
        <h1>agentctl</h1>
        <span className={connected ? 'conn ok' : 'conn down'}>
          {connected ? 'live' : 'reconnecting…'}
        </span>
      </header>
      <main className="main">
        <AgentList sessions={sessions} selectedId={selectedId} onSelect={setSelectedId} />
        {selected ? (
          <div className="detail">
            <h2>{selected.id} <BusyIdleBadge status={selected.status} /></h2>
            <p className="muted">detail view arrives in Phase G</p>
          </div>
        ) : (
          <div className="detail empty">Select an agent</div>
        )}
      </main>
    </div>
  );
}
```

- [ ] **Step 2: Live verify (real daemon)**

Run:
```bash
cd web && npm run build && cd ..
go build -o bin/agentctl ./cmd/agentctl && ./bin/agentctl daemon & sleep 1
# create a couple of sessions to populate the list
rm -rf /tmp/demo && git init -q /tmp/demo && git -C /tmp/demo commit -q --allow-empty -m init
./bin/agentctl start DEMO-1 --type development --repo /tmp/demo
./bin/agentctl start --type buildkite-debug --repo /tmp/demo
curl -s localhost:8765/ | grep -q '_astro' && echo "UI served"
echo "open http://localhost:8765 in a browser — list should show DEMO-1 + the buildkite-debug session, 'live' indicator, selecting a row shows the placeholder detail"
# cleanup
./bin/agentctl done DEMO-1 --force; tmux kill-server 2>/dev/null; kill %1
```
Expected: `UI served` printed; in the browser the list shows both sessions with badges and a `live` indicator; selecting a row shows the placeholder detail.

- [ ] **Step 3: Commit**

```bash
git add web/src/components/Dashboard.tsx
git commit -m "feat(web): dashboard root island with SSE + selection"
```

---

## Phase G — Detail, create, terminate

### Task G1: TerminateControls

**Files:**
- Create: `web/src/components/TerminateControls.tsx`

- [ ] **Step 1: Implement**

Create `web/src/components/TerminateControls.tsx`:
```tsx
import { useState } from 'react';
import { cleanup, ApiError } from '../lib/api';

export default function TerminateControls({ id, onDone }: { id: string; onDone: () => void }) {
  const [busy, setBusy] = useState(false);
  const [guard, setGuard] = useState<string | null>(null);
  const [hard, setHard] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  async function run(force: boolean) {
    setBusy(true);
    setErr(null);
    try {
      await cleanup(id, force, hard);
      onDone();
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        setGuard(e.message); // uncommitted/unpushed guard — offer force
      } else {
        setErr(e instanceof Error ? e.message : String(e));
      }
    } finally {
      setBusy(false);
    }
  }

  if (guard) {
    return (
      <div className="terminate guard">
        <p className="warn">{guard}</p>
        <label>
          <input type="checkbox" checked={hard} onChange={(e) => setHard(e.target.checked)} />{' '}
          also hard-delete the record
        </label>
        <div className="actions">
          <button className="danger" disabled={busy} onClick={() => run(true)}>
            Force terminate (remove worktree + branch)
          </button>
          <button disabled={busy} onClick={() => setGuard(null)}>Cancel</button>
        </div>
      </div>
    );
  }

  return (
    <div className="terminate">
      <button className="danger" disabled={busy} onClick={() => run(false)}>Terminate</button>
      {err && <span className="warn"> {err}</span>}
    </div>
  );
}
```

- [ ] **Step 2: Typecheck + commit**

Run: `cd web && npx tsc --noEmit`
Expected: no errors.
```bash
git add web/src/components/TerminateControls.tsx
git commit -m "feat(web): terminate controls with 409 guard → force/hard"
```

### Task G2: AgentDetail (live output poll + send + history)

**Files:**
- Create: `web/src/components/AgentDetail.tsx`

- [ ] **Step 1: Implement**

Create `web/src/components/AgentDetail.tsx`:
```tsx
import { useEffect, useRef, useState } from 'react';
import type { Session } from '../lib/types';
import { getOutput, sendInput } from '../lib/api';
import EventTimeline from './EventTimeline';
import TerminateControls from './TerminateControls';
import BusyIdleBadge from './BusyIdleBadge';

export default function AgentDetail({ session, onClosed }: { session: Session; onClosed: () => void }) {
  const [output, setOutput] = useState('');
  const [msg, setMsg] = useState('');
  const [sending, setSending] = useState(false);
  const preRef = useRef<HTMLPreElement>(null);

  // Poll the live terminal pane every 2s while this detail is open.
  useEffect(() => {
    let alive = true;
    const poll = async () => {
      try {
        const o = await getOutput(session.id, 200);
        if (alive) setOutput(o);
      } catch { /* session may have ended; SSE will drop it from the list */ }
    };
    poll();
    const t = setInterval(poll, 2000);
    return () => { alive = false; clearInterval(t); };
  }, [session.id]);

  useEffect(() => {
    if (preRef.current) preRef.current.scrollTop = preRef.current.scrollHeight;
  }, [output]);

  async function send() {
    if (!msg.trim()) return;
    setSending(true);
    try {
      await sendInput(session.id, msg);
      setMsg('');
    } catch { /* surfaced via list status / SSE */ } finally {
      setSending(false);
    }
  }

  return (
    <div className="detail">
      <div className="detail-head">
        <h2>{session.id} <BusyIdleBadge status={session.status} /></h2>
        <code className="muted">
          type: {session.type} · repo: {session.repo}{session.worktree && ` · ${session.worktree}`}
        </code>
        <TerminateControls id={session.id} onDone={onClosed} />
      </div>

      <section>
        <h3>Live output</h3>
        <pre className="pane" ref={preRef}>{output || '(no output captured yet)'}</pre>
      </section>

      <section className="sendbox">
        <input
          value={msg}
          onChange={(e) => setMsg(e.target.value)}
          placeholder="Send a message to this agent…"
          onKeyDown={(e) => { if (e.key === 'Enter') send(); }}
        />
        <button disabled={sending} onClick={send}>Send</button>
      </section>

      <section>
        <h3>History</h3>
        <EventTimeline events={session.events} />
        {session.worktree && (
          <p className="muted">Attach in a terminal: <code>agentctl attach {session.id}</code></p>
        )}
      </section>
    </div>
  );
}
```

- [ ] **Step 2: Typecheck + commit**

Run: `cd web && npx tsc --noEmit`
Expected: no errors.
```bash
git add web/src/components/AgentDetail.tsx
git commit -m "feat(web): agent detail — live output, send box, history, attach hint"
```

### Task G3: NewAgentModal (type-aware create)

**Files:**
- Create: `web/src/components/NewAgentModal.tsx`

- [ ] **Step 1: Implement**

Create `web/src/components/NewAgentModal.tsx`:
```tsx
import { useState } from 'react';
import { spawn, ApiError } from '../lib/api';

const TYPES = ['development', 'analysis', 'spike', 'pr-review', 'buildkite-debug', 'test-run', 'env-test', 'other'];

export default function NewAgentModal({ onClose, onCreated }: {
  onClose: () => void;
  onCreated: (id: string) => void;
}) {
  const [type, setType] = useState('development');
  const [ticket, setTicket] = useState('');
  const [repo, setRepo] = useState('');
  const [branch, setBranch] = useState('');
  const [pr, setPr] = useState('');
  const [worktree, setWorktree] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const showBranch = type === 'development' || type === 'pr-review';
  const showPr = type === 'pr-review';
  const showWorktree = type === 'analysis' || type === 'spike';

  async function submit() {
    setErr(null);
    if (!repo.trim()) { setErr('repo is required'); return; }
    if (type === 'pr-review' && !pr.trim() && !branch.trim()) {
      setErr('pr-review needs a PR number or a branch'); return;
    }
    setBusy(true);
    try {
      const s = await spawn({ type, ticket, repo, branch, pr, worktree });
      onCreated(s.id);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e instanceof Error ? e.message : String(e)));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h2>New agent</h2>
        <label>Type
          <select value={type} onChange={(e) => setType(e.target.value)}>
            {TYPES.map((t) => <option key={t} value={t}>{t}</option>)}
          </select>
        </label>
        <label>Repo path
          <input value={repo} onChange={(e) => setRepo(e.target.value)} placeholder="/Users/…/the-monorepo" />
        </label>
        <label>Ticket (optional)
          <input value={ticket} onChange={(e) => setTicket(e.target.value)} placeholder="PROJ-350" />
        </label>
        {showBranch && (
          <label>Branch {type === 'pr-review' ? '(checkout target)' : '(new)'}
            <input value={branch} onChange={(e) => setBranch(e.target.value)} />
          </label>
        )}
        {showPr && (
          <label>PR number/url
            <input value={pr} onChange={(e) => setPr(e.target.value)} />
          </label>
        )}
        {showWorktree && (
          <label>
            <input type="checkbox" checked={worktree} onChange={(e) => setWorktree(e.target.checked)} />{' '}
            create scratch worktree
          </label>
        )}
        {err && <p className="warn">{err}</p>}
        <div className="actions">
          <button disabled={busy} onClick={submit}>Create</button>
          <button onClick={onClose}>Cancel</button>
        </div>
      </div>
    </div>
  );
}
```

- [ ] **Step 2: Typecheck + commit**

Run: `cd web && npx tsc --noEmit`
Expected: no errors.
```bash
git add web/src/components/NewAgentModal.tsx
git commit -m "feat(web): type-aware new-agent modal"
```

### Task G4: Wire Dashboard to real detail + create modal + final styles

**Files:**
- Modify: `web/src/components/Dashboard.tsx`
- Modify: `web/src/styles/app.css`

- [ ] **Step 1: Final Dashboard**

Replace `web/src/components/Dashboard.tsx`:
```tsx
import { useEffect, useState } from 'react';
import type { Session } from '../lib/types';
import { listSessions, subscribeSessions } from '../lib/api';
import AgentList from './AgentList';
import AgentDetail from './AgentDetail';
import NewAgentModal from './NewAgentModal';

export default function Dashboard() {
  const [sessions, setSessions] = useState<Session[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [connected, setConnected] = useState(false);
  const [showCreate, setShowCreate] = useState(false);

  useEffect(() => {
    listSessions().then(setSessions).catch(() => { /* SSE will populate */ });
    const unsub = subscribeSessions(
      setSessions,
      () => setConnected(false),
      () => setConnected(true),
    );
    return unsub;
  }, []);

  const selected = sessions.find((s) => s.id === selectedId) ?? null;

  return (
    <div className="layout">
      <header className="topbar">
        <h1>agentctl</h1>
        <span className={connected ? 'conn ok' : 'conn down'}>
          {connected ? 'live' : 'reconnecting…'}
        </span>
        <button onClick={() => setShowCreate(true)}>+ New agent</button>
      </header>
      <main className="main">
        <AgentList sessions={sessions} selectedId={selectedId} onSelect={setSelectedId} />
        {selected
          ? <AgentDetail session={selected} onClosed={() => setSelectedId(null)} />
          : <div className="detail empty">Select an agent</div>}
      </main>
      {showCreate && (
        <NewAgentModal
          onClose={() => setShowCreate(false)}
          onCreated={(id) => { setShowCreate(false); setSelectedId(id); }}
        />
      )}
    </div>
  );
}
```

- [ ] **Step 2: Fill in the stylesheet**

Replace `web/src/styles/app.css`:
```css
:root {
  font-family: ui-sans-serif, system-ui, -apple-system, sans-serif;
  color-scheme: light dark;
  --busy: #1a7f37; --attention: #b08800; --idle: #6b7280; --error: #cf222e;
}
* { box-sizing: border-box; }
body { margin: 0; }
.layout { display: flex; flex-direction: column; height: 100vh; }
.topbar { display: flex; align-items: center; gap: 1rem; padding: .6rem 1rem; border-bottom: 1px solid #8884; }
.topbar h1 { font-size: 1.1rem; margin: 0; }
.topbar button { margin-left: auto; }
.conn { font-size: .8rem; padding: .1rem .5rem; border-radius: 1rem; }
.conn.ok { background: #1a7f3722; color: var(--busy); }
.conn.down { background: #cf222e22; color: var(--error); }
.main { display: grid; grid-template-columns: minmax(420px, 1fr) minmax(420px, 1fr); gap: 1rem; padding: 1rem; overflow: auto; flex: 1; }
.list table { width: 100%; border-collapse: collapse; font-size: .9rem; }
.list th, .list td { text-align: left; padding: .35rem .5rem; border-bottom: 1px solid #8882; }
.list tbody tr { cursor: pointer; }
.list tbody tr:hover { background: #8881; }
.list tbody tr.sel { background: #2f81f733; }
.list.empty, .detail.empty { color: var(--idle); padding: 2rem; }
.badge { font-size: .75rem; padding: .1rem .5rem; border-radius: 1rem; color: #fff; }
.badge.busy { background: var(--busy); }
.badge.attention { background: var(--attention); }
.badge.idle { background: var(--idle); }
.badge.error { background: var(--error); }
.muted { color: var(--idle); }
.warn { color: var(--error); }
.detail { display: flex; flex-direction: column; gap: 1rem; min-width: 0; }
.detail-head { display: flex; flex-direction: column; gap: .4rem; }
.pane { background: #0b0b0b; color: #d6d6d6; padding: .75rem; border-radius: .4rem; height: 280px; overflow: auto; white-space: pre-wrap; font-size: .82rem; }
.sendbox { display: flex; gap: .5rem; }
.sendbox input { flex: 1; padding: .4rem; }
.timeline { list-style: none; padding: 0; margin: 0; font-size: .85rem; }
.timeline li { padding: .25rem 0; border-bottom: 1px solid #8882; }
.timeline time { color: var(--idle); }
.terminate { display: flex; flex-direction: column; gap: .4rem; }
.terminate .actions { display: flex; gap: .5rem; }
.danger { color: #fff; background: var(--error); border: none; padding: .4rem .7rem; border-radius: .3rem; cursor: pointer; }
.modal-backdrop { position: fixed; inset: 0; background: #0007; display: grid; place-items: center; }
.modal { background: Canvas; color: CanvasText; padding: 1.2rem; border-radius: .5rem; width: min(420px, 92vw); display: flex; flex-direction: column; gap: .6rem; }
.modal label { display: flex; flex-direction: column; gap: .25rem; font-size: .9rem; }
.modal input, .modal select { padding: .4rem; }
.modal .actions { display: flex; gap: .5rem; justify-content: flex-end; }
```

- [ ] **Step 3: Build + typecheck + frontend tests**

Run: `cd web && npx tsc --noEmit && npm run build && npm test`
Expected: typecheck clean, build succeeds (writes `dist/`), Vitest green.

- [ ] **Step 4: Commit**

```bash
git add web/src/components/Dashboard.tsx web/src/styles/app.css
git commit -m "feat(web): wire dashboard to detail + create modal + styling"
```

---

## Phase H — Integration & docs

### Task H1: Full end-to-end verification

**Files:** none (verification only)

- [ ] **Step 1: Build everything and run the whole backend suite**

Run:
```bash
cd /Users/srajan.pathak/workspace/personal/agentctl
make release            # npm build → embed → go build
go test ./...           # all Go packages (store needs Docker → make mongo-up first)
cd web && npm test && cd ..
```
Expected: `make release` produces `bin/agentctl` with the real UI embedded; all Go tests pass; Vitest passes.

- [ ] **Step 2: Manual browser smoke (the one human-verified pass the spec calls for)**

Run:
```bash
make mongo-up
./bin/agentctl daemon & sleep 1
rm -rf /tmp/demo && git init -q /tmp/demo && git -C /tmp/demo commit -q --allow-empty -m init
open http://localhost:8765
```
Verify in the browser, in order:
1. **List + live:** header shows `live`; the list is empty initially.
2. **Create:** click “+ New agent”, choose `development`, repo `/tmp/demo`, ticket `DEMO-1`, Create → the row appears **without a manual refresh** (SSE push), badge shows **Busy/Starting**.
3. **Create no-worktree:** “+ New agent”, type `buildkite-debug`, repo `/tmp/demo`, Create → auto-id `buildkitedebug-xxxx` row appears, no branch.
4. **Detail + history + output:** click DEMO-1 → detail shows metadata, the event timeline, the live `pre` pane updating, and the `agentctl attach DEMO-1` hint.
5. **Send:** type a message + Send → no error (the keystrokes reach the tmux pane; visible in `tail`/attach).
6. **Terminate:** click Terminate on DEMO-1. If it has unpushed work it shows the guard message → Force terminate removes it; the row disappears via SSE.
7. **Busy/idle:** leave a `working` session idle; within the poller window the badge reflects status changes pushed over SSE.

Tear down:
```bash
./bin/agentctl ls; tmux kill-server 2>/dev/null; kill %1
```
Expected: all seven behaviors confirmed.

- [ ] **Step 3: Commit the built UI is NOT committed (dist is gitignored)** — nothing to commit here; this is a verification gate.

### Task H2: README — Web GUI section

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Add a "Web GUI" section**

Append to `README.md` a section documenting:
- **Build & run:** `make release` (builds the Astro UI, embeds it, builds the binary), then `agentctl daemon`; open `http://localhost:8765`.
- **What it does:** lists agents live (SSE), create via “+ New agent” (type-aware form), open an agent for live output + full event history + send-message, busy/idle badges, and Terminate (with Force/Hard for the uncommitted/unpushed guard).
- **Dev workflow:** run `agentctl daemon` (:8765) in one terminal and `make ui-dev` (Astro on :4321, proxying API + SSE to the daemon) in another; edit under `web/src/` with HMR.
- **Tests:** `make web-test` (Vitest) for the frontend; `go test ./...` covers the daemon (hub/SSE/static).
- **Note:** the UI is embedded at build time — after changing `web/`, re-run `make release` (or `make ui`) and restart the daemon. Browsers can't `tmux attach`; the detail view shows the `agentctl attach <id>` command instead.

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs: README web GUI section (build, usage, dev workflow)"
```

---

## Self-review against the spec

**Spec coverage** (`2026-06-01-agentctl-gui-design.md`):
- §1 list agents — `AgentList` (Task F1), fed by SSE (F2). ✅
- §1 create agents — `NewAgentModal` → `spawn` (G3). ✅
- §1 manage (send/read) — `AgentDetail` send box (`sendInput`) + live output poll (`getOutput`) (G2). ✅
- §1 full history — `EventTimeline` over `events[]` (F1/G2). ✅
- §1 monitor loop / live updates — SSE endpoint (C2) + `subscribeSessions` + Dashboard (C/F2). ✅
- §1 busy/idle — `status.ts` mapping (E2) + `BusyIdleBadge` (F1). ✅
- §1 terminate incl. worktree — `TerminateControls` → `cleanup` with force/hard + 409 guard (G1). ✅
- §2 decisions — static embed (A), SSE (C), React islands (D/F/G), output short-poll (G2), dev proxy (D1). ✅
- §3.1 hub — Task B1. §3.2 SSE — C2. §3.3 static embed + fallback — A1/A2. §3.4 build/dev — A3/D1. ✅
- §4 file structure — matches Tasks A1, D, E, F, G. ✅
- §4.1 data flow (SSE list + one-shot first fetch + output poll) — F2/G2. ✅
- §4.2 busy/idle table — E2 mirrors it exactly. ✅
- §4.3 create form (conditional fields, 400/409) — G3. ✅
- §4.4 terminate (force/hard, attach hint) — G1/G2. ✅
- §5 error handling — SSE reconnect banner (F2 `connected`), spawn errors (G3), cleanup 409 (G1), output 404 swallowed (G2). ✅
- §6 testing — Go hub/sse/static/poller-OnChange (B1/C2/A2/C1); Vitest status/api (E2/E3); manual smoke (H1). ✅
- §7 out of scope — respected (no auth, no in-browser attach, no config UI). ✅

**Placeholder scan:** No TBD/TODO; every code step shows complete code; every run step states expected output. The npm dependency versions in D1 are concrete (Astro 5 / React 19 / Vitest 3 era); `npm install` resolves the lockfile.

**Type consistency:** `Session`/`AgentEvent`/`Status` (E1) match the Go JSON tags exactly (`tmux_session`, `last_pane_excerpt`, `events` nullable). `busyIdle` (E2) returns `{label,kind}` consumed by `BusyIdleBadge` (F1). `api.ts` exports (`listSessions/getSession/spawn/cleanup/sendInput/getOutput/subscribeSessions/ApiError/SpawnParams`) match every call site in F2/G1/G2/G3. Go: `hub.subscribe/publish` (B1) used by `sse.go` (C2) and `notify()` (C3); `poller.Poller.OnChange` (C1) set in `NewServer` (C1); `registerStatic`/`handleEventsStream` registered in `router()` (A2/C2). The daemon `NewServer` signature is unchanged, so `cli/daemon.go` needs no edits.

**Scope:** One subsystem (GUI + the daemon's SSE/static additions). Single plan is appropriate.
