# Warden Track-1 Security Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Three independent hardening fixes for the public, auth-less warden daemon: `0o700` data dirs (new + existing), HTTP slowloris/body/write-timeout guards that don't break long-lived streams, and a refuse-non-loopback-bind default.

**Architecture:** Pure, unit-tested helpers (`IsLoopbackHost`, `HardenDataDir`, `isStreamingPath`, the middleware factories) plus small edits at the permission-creation sites and the daemon startup path. No new packages; follows existing patterns (`config` env helpers, `daemon` middleware/`writeJSON`).

**Tech Stack:** Go 1.26, go-chi router, cobra CLI.

**Spec:** `docs/superpowers/specs/2026-06-09-warden-track1-hardening-design.md`

**Conventions (read before starting):**
- `config` env helpers: `env("NAME")` reads `WARDEN_NAME` then `AGENTCTL_NAME`; boolean flags follow `notifyEnabled()` (off-by-default → on for `1/on/true`).
- `daemon` HTTP helpers: `writeErr(w, code, msg)`, `writeJSON(w, code, v)`; middlewares are plain `func(http.Handler) http.Handler` registered via `r.Use(...)` in `router()` (`internal/daemon/api.go`).
- Run one Go test: `go test ./internal/<pkg>/ -run TestName -v`.
- Work from the worktree root (you are already in it). Branch `worktree-track1-hardening`, based on `main`. Commit after each task.

---

## File Structure

**Create:**
- `internal/daemon/harden.go` — `HardenDataDir(dataDir)` + `chmodDirIfExists`.
- `internal/daemon/harden_test.go`
- `internal/daemon/middleware.go` — `maxBytes(n)`, `writeTimeout(d)`, `isStreamingPath`, the size/timeout consts.
- `internal/daemon/middleware_test.go`

**Modify:**
- `internal/config/config.go` — `AllowNonLoopback` field + `allowNonLoopback()` + pure `IsLoopbackHost(addr)`.
- `internal/config/config_test.go` — tests for both.
- `internal/store/file.go:41,44` — `0o755 → 0o700`.
- `internal/store/file_test.go` — assert created dirs are `0o700`.
- `internal/ctxstore/ctxstore.go:56` — `0o755 → 0o700`.
- `internal/mailbox/mailbox.go:41` — `0o755 → 0o700`.
- `internal/pipeline/store.go:27` — `0o755 → 0o700`.
- `internal/lifecycle/lifecycle.go:580,997` — `mkdir -p` → `mkdir -m 700 -p`.
- `internal/daemon/server.go` — `http.Server` timeout/header fields.
- `internal/daemon/api.go` — register the two middlewares in `router()`.
- `internal/cli/daemon.go` — call `HardenDataDir` + enforce the bind guard.

---

## Task 1: `config.IsLoopbackHost` + `AllowNonLoopback` flag

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing tests (append to `config_test.go`)**

```go
func TestIsLoopbackHost(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8765": true,
		"localhost:8765": true,
		"[::1]:8765":     true,
		"127.0.0.1":      true, // bare host, no port
		":8765":          false, // empty host = all interfaces
		"0.0.0.0:8765":   false,
		"192.168.1.5:8765": false,
		"example.com:8765": false, // unresolved hostname → fail safe
	}
	for addr, want := range cases {
		if got := IsLoopbackHost(addr); got != want {
			t.Fatalf("IsLoopbackHost(%q)=%v want %v", addr, got, want)
		}
	}
}

func TestAllowNonLoopbackFlag(t *testing.T) {
	t.Setenv("WARDEN_ALLOW_NONLOOPBACK", "")
	t.Setenv("AGENTCTL_ALLOW_NONLOOPBACK", "")
	if Load().AllowNonLoopback {
		t.Fatal("should default OFF")
	}
	t.Setenv("WARDEN_ALLOW_NONLOOPBACK", "1")
	if !Load().AllowNonLoopback {
		t.Fatal("WARDEN_ALLOW_NONLOOPBACK=1 should enable")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/config/ -run 'TestIsLoopbackHost|TestAllowNonLoopback' -v`
Expected: FAIL — `undefined: IsLoopbackHost` / `AllowNonLoopback`.

- [ ] **Step 3: Implement in `config.go`**

Add `"net"` to the import block (alongside `os`, `path/filepath`, `strconv`, `strings`).

Add the field to the `Config` struct, after `MetricsEnabled bool`:

```go
	MetricsEnabled     bool
	AllowNonLoopback   bool
```

Add the helper after `metricsEnabled()`:

```go
// allowNonLoopback reads WARDEN_ALLOW_NONLOOPBACK (legacy AGENTCTL_ALLOW_NONLOOPBACK);
// OFF by default, on only for 1/on/true. Gates binding the auth-less daemon to a
// non-loopback address.
func allowNonLoopback() bool {
	switch strings.ToLower(env("ALLOW_NONLOOPBACK")) {
	case "1", "on", "true":
		return true
	}
	return false
}

// IsLoopbackHost reports whether addr (host:port, or a bare host) binds only the
// loopback interface. An empty host (e.g. ":8765") binds all interfaces and is
// NOT loopback. Unresolvable hostnames are treated as non-loopback (fail safe).
// No DNS lookups.
func IsLoopbackHost(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // no port present
	}
	host = strings.TrimSpace(host)
	switch host {
	case "":
		return false
	case "localhost":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
```

Wire the flag into `Load()`'s returned `Config{...}` literal, after the `MetricsEnabled:` line:

```go
		MetricsEnabled:     metricsEnabled(),
		AllowNonLoopback:   allowNonLoopback(),
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/config/ -run 'TestIsLoopbackHost|TestAllowNonLoopback' -v`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/config/config.go internal/config/config_test.go
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): IsLoopbackHost + WARDEN_ALLOW_NONLOOPBACK flag"
```

---

## Task 2: Tighten data-dir creation modes (`0o755 → 0o700`)

**Files:**
- Modify: `internal/store/file.go:41,44`, `internal/ctxstore/ctxstore.go:56`, `internal/mailbox/mailbox.go:41`, `internal/pipeline/store.go:27`
- Test: `internal/store/file_test.go`

- [ ] **Step 1: Write the failing test (append to `internal/store/file_test.go`)**

```go
func TestNewFileStoreDirsAre0700(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close(context.Background())
	for _, sub := range []string{"sessions", "closed"} {
		info, err := os.Stat(filepath.Join(dir, sub))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %o, want 700", sub, info.Mode().Perm())
		}
	}
}
```

If `file_test.go` doesn't already import `os`, `path/filepath`, or `context`, add them (check the existing imports first; `NewFileStore`/`Close` are already used by other tests in the package, so most are present).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/store/ -run TestNewFileStoreDirsAre0700 -v`
Expected: FAIL — mode = 755, want 700.

- [ ] **Step 3: Change the four creation sites to `0o700`**

`internal/store/file.go` — both lines (41 and 44):
```go
	if err := os.MkdirAll(fs.sessions, 0o700); err != nil {
```
```go
	if err := os.MkdirAll(fs.closed, 0o700); err != nil {
```

`internal/ctxstore/ctxstore.go:56`:
```go
	if err := os.MkdirAll(dir, 0o700); err != nil {
```

`internal/mailbox/mailbox.go:41`:
```go
	if err := os.MkdirAll(dir, 0o700); err != nil {
```

`internal/pipeline/store.go:27`:
```go
	if err := os.MkdirAll(dir, 0o700); err != nil {
```

- [ ] **Step 4: Run to verify it passes (and nothing regressed)**

Run: `go test ./internal/store/ ./internal/ctxstore/ ./internal/mailbox/ ./internal/pipeline/ -run 'TestNewFileStoreDirsAre0700|Test' 2>&1 | tail -8`
Expected: all four packages `ok` (the new test passes; existing tests unaffected).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/store/file.go internal/store/file_test.go
git add internal/store/file.go internal/store/file_test.go internal/ctxstore/ctxstore.go internal/mailbox/mailbox.go internal/pipeline/store.go
git commit -m "feat(store): create data dirs 0o700 (sessions/closed/context/inbox/pipelines)"
```

---

## Task 3: Tighten the prompts dir (`mkdir -m 700`)

**Files:**
- Modify: `internal/lifecycle/lifecycle.go:580,997`

**Context:** The `prompts` dir is created lazily on first spawn via the `Runner` (so it can't be a Go `os.MkdirAll`; tests use a `FakeRunner` that records argv). Both call sites currently run `mkdir -p <PromptsDir>`. Adding `-m 700` sets owner-only mode on the created dir. There is no unit assertion on shell argv here, so this is a direct edit verified by build + the package suite; the startup `HardenDataDir` (Task 4) is the belt-and-suspenders for an already-existing prompts dir.

- [ ] **Step 1: Edit both call sites**

`internal/lifecycle/lifecycle.go:580` — change:
```go
		if out, err := l.run.Run(ctx, "", "mkdir", "-p", l.PromptsDir); err != nil {
```
to:
```go
		if out, err := l.run.Run(ctx, "", "mkdir", "-m", "700", "-p", l.PromptsDir); err != nil {
```

`internal/lifecycle/lifecycle.go:997` — change:
```go
	if out, err := l.run.Run(ctx, "", "mkdir", "-p", l.PromptsDir); err != nil {
```
to:
```go
	if out, err := l.run.Run(ctx, "", "mkdir", "-m", "700", "-p", l.PromptsDir); err != nil {
```

- [ ] **Step 2: Build + run the lifecycle suite**

Run: `go build ./internal/lifecycle/ && go test ./internal/lifecycle/ 2>&1 | tail -5`
Expected: build ok; tests `ok` (the `FakeRunner` accepts any argv; if a test asserts the exact `mkdir` argv it will need the two new args — update it to match if so, otherwise no change needed).

- [ ] **Step 3: Commit**

```bash
gofmt -w internal/lifecycle/lifecycle.go
git add internal/lifecycle/lifecycle.go
git commit -m "feat(lifecycle): create prompts dir mode 0700 (mkdir -m 700)"
```

---

## Task 4: `daemon.HardenDataDir` (chmod existing dirs)

**Files:**
- Create: `internal/daemon/harden.go`
- Test: `internal/daemon/harden_test.go`

- [ ] **Step 1: Write the failing test**

```go
package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHardenDataDir(t *testing.T) {
	root := t.TempDir()
	// dataDir + two existing subdirs at 0o755; "inbox" intentionally absent.
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"sessions", "closed"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := HardenDataDir(root); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{root, filepath.Join(root, "sessions"), filepath.Join(root, "closed")} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %o, want 700", p, info.Mode().Perm())
		}
	}
	// Absent subdir must not have been created.
	if _, err := os.Stat(filepath.Join(root, "inbox")); !os.IsNotExist(err) {
		t.Fatalf("inbox should not exist, stat err = %v", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/daemon/ -run TestHardenDataDir -v`
Expected: FAIL — `undefined: HardenDataDir`.

- [ ] **Step 3: Implement `internal/daemon/harden.go`**

```go
package daemon

import (
	"os"
	"path/filepath"
)

// hardenedSubdirs are warden's known data subdirectories under the data root.
// HardenDataDir tightens each that exists; new dirs are created 0o700 at their
// own creation sites (the stores; the prompts dir via `mkdir -m 700`).
var hardenedSubdirs = []string{"sessions", "closed", "context", "inbox", "pipelines", "prompts", "metrics"}

// HardenDataDir chmods the data root and each known subdirectory that already
// exists to 0o700 (owner-only), so pre-existing installs created at 0o755 are
// tightened. Missing dirs are skipped; the first real error is returned.
func HardenDataDir(dataDir string) error {
	if err := chmodDirIfExists(dataDir); err != nil {
		return err
	}
	for _, sub := range hardenedSubdirs {
		if err := chmodDirIfExists(filepath.Join(dataDir, sub)); err != nil {
			return err
		}
	}
	return nil
}

// chmodDirIfExists sets p to 0o700 when it exists and is a directory; a missing
// path is a no-op (not an error).
func chmodDirIfExists(p string) error {
	info, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}
	return os.Chmod(p, 0o700)
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/daemon/ -run TestHardenDataDir -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/daemon/harden.go internal/daemon/harden_test.go
git add internal/daemon/harden.go internal/daemon/harden_test.go
git commit -m "feat(daemon): HardenDataDir chmods existing data dirs to 0o700"
```

---

## Task 5: HTTP middleware (body cap + write timeout) + server timeouts

**Files:**
- Create: `internal/daemon/middleware.go`, `internal/daemon/middleware_test.go`
- Modify: `internal/daemon/server.go`, `internal/daemon/api.go`

- [ ] **Step 1: Write the failing tests (`internal/daemon/middleware_test.go`)**

```go
package daemon

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIsStreamingPath(t *testing.T) {
	cases := map[string]bool{
		"/events/stream":                 true,
		"/sessions/abc/attach":           true,
		"/sessions/abc/messages/wait":    true,
		"/metrics":                       false,
		"/sessions/abc/output":           false,
		"/sessions":                      false,
	}
	for p, want := range cases {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		if got := isStreamingPath(r); got != want {
			t.Fatalf("isStreamingPath(%q)=%v want %v", p, got, want)
		}
	}
}

func TestWriteTimeoutTimesOutNonStreaming(t *testing.T) {
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	h := writeTimeout(50 * time.Millisecond)(slow)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
}

func TestWriteTimeoutBypassesStreaming(t *testing.T) {
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	h := writeTimeout(50 * time.Millisecond)(slow)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/events/stream", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("streaming path should not time out; code = %d, want 200", rec.Code)
	}
}

func TestMaxBytesRejectsOversizedBody(t *testing.T) {
	var readErr error
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
		if readErr != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	h := maxBytes(10)(handler) // 10-byte cap
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/spawn", strings.NewReader(strings.Repeat("x", 100)))
	h.ServeHTTP(rec, req)
	if readErr == nil {
		t.Fatal("expected body read to fail past the cap")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code = %d, want 413", rec.Code)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/daemon/ -run 'TestIsStreamingPath|TestWriteTimeout|TestMaxBytes' -v`
Expected: FAIL — `undefined: isStreamingPath` / `writeTimeout` / `maxBytes`.

- [ ] **Step 3: Implement `internal/daemon/middleware.go`**

```go
package daemon

import (
	"net/http"
	"strings"
	"time"
)

const (
	// maxBodyBytes caps a request body (JSON POSTs); GETs and the WS upgrade
	// read no body, so this is a no-op for them.
	maxBodyBytes int64 = 1 << 20
	// writeTimeoutDur bounds non-streaming handler execution. Streaming routes
	// (SSE, WS attach, message long-poll) are exempt — see isStreamingPath.
	writeTimeoutDur = 30 * time.Second
)

// isStreamingPath reports whether a request targets a long-lived endpoint that
// must NOT be wrapped in http.TimeoutHandler (it buffers the response and breaks
// Flush/Hijack): the SSE stream, the WS tmux attach, and the message long-poll.
func isStreamingPath(r *http.Request) bool {
	p := r.URL.Path
	return p == "/events/stream" ||
		strings.HasSuffix(p, "/attach") ||
		strings.HasSuffix(p, "/messages/wait")
}

// maxBytes returns middleware that caps each request body at n bytes.
func maxBytes(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}

// writeTimeout returns middleware that bounds handler execution at d, except for
// streaming paths (which would break under http.TimeoutHandler's buffering).
func writeTimeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		timed := http.TimeoutHandler(next, d, `{"error":"request timed out"}`)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isStreamingPath(r) {
				next.ServeHTTP(w, r)
				return
			}
			timed.ServeHTTP(w, r)
		})
	}
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/daemon/ -run 'TestIsStreamingPath|TestWriteTimeout|TestMaxBytes' -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Register the middlewares in `router()`**

In `internal/daemon/api.go`, the `router()` function starts:
```go
	r := chi.NewRouter()
	r.Use(recoverMiddleware)
```
Add the two new middlewares right after `recoverMiddleware`:
```go
	r := chi.NewRouter()
	r.Use(recoverMiddleware)
	r.Use(maxBytes(maxBodyBytes))
	r.Use(writeTimeout(writeTimeoutDur))
```

- [ ] **Step 6: Add the `http.Server` timeout fields**

In `internal/daemon/server.go`, find:
```go
	httpSrv := &http.Server{Addr: addr, Handler: s.router()}
```
Replace with:
```go
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           s.router(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
```
(`time` is already imported in `server.go`.)

- [ ] **Step 7: Build + run the daemon suite**

Run: `go build ./internal/daemon/ && go test ./internal/daemon/ -timeout 120s 2>&1 | tail -6`
Expected: build ok; package `ok` (existing route/SSE tests still pass — the streaming bypass keeps `/events/stream` working).

- [ ] **Step 8: Commit**

```bash
gofmt -w internal/daemon/middleware.go internal/daemon/middleware_test.go internal/daemon/api.go internal/daemon/server.go
git add internal/daemon/middleware.go internal/daemon/middleware_test.go internal/daemon/api.go internal/daemon/server.go
git commit -m "feat(daemon): body cap + non-streaming write timeout + server header/idle timeouts"
```

---

## Task 6: Wire startup hardening + bind guard into the daemon command

**Files:**
- Modify: `internal/cli/daemon.go`

**Context:** `RunE` loads `cfg`, applies the `--addr` override, then builds the stores and calls `srv.ListenAndServe(ctx, cfg.Addr)`. The bind guard must run before binding; `HardenDataDir` must run after the stores create their dirs (so they exist to chmod).

- [ ] **Step 1: Add the bind guard right after the `--addr` override**

In `internal/cli/daemon.go`, find:
```go
				cfg := config.Load()
				if a, _ := cmd.Flags().GetString("addr"); a != "" {
					cfg.Addr = a
				}
```
Add immediately after it:
```go
				if !config.IsLoopbackHost(cfg.Addr) && !cfg.AllowNonLoopback {
					return fmt.Errorf("refusing to bind non-loopback address %q: the warden daemon has no authentication; set WARDEN_ALLOW_NONLOOPBACK=1 to override", cfg.Addr)
				}
```
Add `"fmt"` to the import block of `daemon.go` if not already present.

- [ ] **Step 2: Call `HardenDataDir` after the stores are constructed**

In the same `RunE`, the stores are built like:
```go
				pstore, err := pipeline.NewStore(filepath.Join(cfg.DataDir, "pipelines"))
				if err != nil {
					return err
				}
```
Add right after that block (the last store constructor, before `srv := daemon.NewServer(...)`):
```go
				if err := daemon.HardenDataDir(cfg.DataDir); err != nil {
					return err
				}
```

- [ ] **Step 3: Build the whole module**

Run: `go build ./...`
Expected: success (no output).

- [ ] **Step 4: Commit**

```bash
gofmt -w internal/cli/daemon.go
git add internal/cli/daemon.go
git commit -m "feat(daemon): enforce loopback-bind guard + harden data dirs on startup"
```

---

## Task 7: Full verification

**Files:** none (verification only)

- [ ] **Step 1: Run the touched packages in isolation, sequentially**

Run: `go test -p 1 -timeout 120s ./internal/config/ ./internal/daemon/ ./internal/store/ ./internal/ctxstore/ ./internal/mailbox/ ./internal/pipeline/ ./internal/lifecycle/ ./internal/cli/ 2>&1 | tail -12`
Expected: all `ok`. (Run sequentially because the machine may be under load; `internal/lifecycle`/`internal/cli` are heavy.)

- [ ] **Step 2: Vet + format**

Run: `gofmt -l internal/ && go vet ./internal/config/ ./internal/daemon/ ./internal/cli/ ./internal/store/ ./internal/ctxstore/ ./internal/mailbox/ ./internal/pipeline/ ./internal/lifecycle/`
Expected: no files listed by gofmt; vet clean.

- [ ] **Step 3: Build the binary**

Run: `go build -o /tmp/wd-harden ./cmd/warden`
Expected: success.

- [ ] **Step 4: Verify the bind guard manually (no daemon left running)**

Run: `WARDEN_DATA_DIR=/tmp/warden-harden WARDEN_ADDR=0.0.0.0:8799 /tmp/wd-harden daemon; echo "exit=$?"`
Expected: prints the "refusing to bind non-loopback address" error and exits non-zero **immediately** (never binds). Then confirm the override path is accepted (this one WOULD start a daemon, so only check it parses — run with a 1s timeout):
Run: `WARDEN_DATA_DIR=/tmp/warden-harden WARDEN_ADDR=0.0.0.0:8799 WARDEN_ALLOW_NONLOOPBACK=1 timeout 1 /tmp/wd-harden daemon; echo "exit=$?"`
Expected: it does NOT print the refusal (it gets past the guard); `timeout` kills it (exit 124) — that's success for this check.

- [ ] **Step 5: Verify existing-dir chmod**

Run: `mkdir -p /tmp/warden-harden-chmod/sessions && chmod 755 /tmp/warden-harden-chmod/sessions && WARDEN_DATA_DIR=/tmp/warden-harden-chmod WARDEN_ADDR=127.0.0.1:8798 timeout 1 /tmp/wd-harden daemon; stat -f '%Lp %N' /tmp/warden-harden-chmod /tmp/warden-harden-chmod/sessions`
Expected: both print `700` (the startup `HardenDataDir` tightened the pre-existing `0o755` dirs). `timeout` killing the daemon (exit 124) is fine.

- [ ] **Step 6: Final commit (if any fixups were needed)**

```bash
git add -A && git commit -m "chore(hardening): verification fixups" || echo "nothing to commit"
```

---

## Notes for the implementer

- **`make install` + daemon restart** is required for these to take effect on the running daemon (the bind guard + startup chmod run at daemon start; the middleware/timeouts are compiled in). Left for the user.
- The bind-guard enforcement line in `RunE` isn't unit-tested directly (it's a thin check over the unit-tested `IsLoopbackHost`); Step 4 verifies it end-to-end against the built binary.
- If `internal/store/file_test.go` or a `lifecycle` test already asserts the exact `mkdir` argv, update that assertion to include `-m 700` rather than leaving it red.
