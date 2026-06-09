# File-Based JSON Store (drop Mongo) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace MongoDB with a file-based JSON store (one JSON file per session) behind the existing `store.Store` interface, so agentctl ships with no Docker/Mongo setup.

**Architecture:** A new `store.FileStore` implements the full 14-method `Store` interface. Each session is one pretty-printed JSON file at `<dataDir>/sessions/<id>.json`; archived sessions move to `<dataDir>/closed/<id>.json`. Writes are atomic (temp file + `os.Rename`); a process-wide `sync.RWMutex` serializes the daemon's concurrent callers (HTTP handlers, poller, classify goroutine). Mongo (driver, compose, testcontainers tests) is deleted; config gains `DataDir` (`AGENTCTL_DATA_DIR`, default `~/.agentctl`) in place of `MongoURI`/`DB`. No consumer of the interface changes.

**Tech Stack:** Go 1.26, stdlib only (`encoding/json`, `os`, `path/filepath`, `sync`), `github.com/stretchr/testify/require` for tests.

**Design spec:** `docs/superpowers/specs/2026-06-02-agentctl-filestore-design.md`

**Ordering rationale (build stays green at every commit):** FileStore is built first as a new, unwired file (Tasks 1–2). Then `types.go`'s bson tags + round-trip test are de-Mongo'd (Task 3) — safe while `mongo.go` still compiles. Then config + daemon are switched to `FileStore` in one commit (Task 4) so no commit references a removed field. Only then is Mongo deleted and `go mod tidy` run (Task 5), since nothing imports the driver by that point. Docs last (Task 6).

---

### Task 1: FileStore core — Insert / Get / List / Archive / Delete + atomic write + id safety

**Files:**
- Create: `internal/store/file.go`
- Test: `internal/store/file_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/store/file_test.go`:

```go
package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// sample returns a representative session. (Moved here from the deleted
// mongo_test.go; file_test.go is now the home for store test fixtures.)
func sample() *Session {
	return &Session{
		ID: "PROJ-350", Ticket: "PROJ-350", TmuxSession: "PROJ-350",
		Repo: "/repo", Worktree: ".worktrees/PROJ-350", Branch: "PROJ-350",
		Status: StatusSpawning,
	}
}

func newFileStore(t *testing.T) *FileStore {
	t.Helper()
	st, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	return st
}

func TestFileInsertGet(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	require.NoError(t, st.Insert(ctx, sample()))

	got, err := st.Get(ctx, "PROJ-350")
	require.NoError(t, err)
	require.Equal(t, StatusSpawning, got.Status)
	require.False(t, got.CreatedAt.IsZero(), "Insert must stamp CreatedAt")
	require.False(t, got.UpdatedAt.IsZero(), "Insert must stamp UpdatedAt")
	require.NotNil(t, got.Events, "Insert must init Events to non-nil")
}

func TestFileInsertDuplicate(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	require.NoError(t, st.Insert(ctx, sample()))
	require.ErrorIs(t, st.Insert(ctx, sample()), ErrExists)
}

func TestFileGetNotFound(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	_, err := st.Get(ctx, "nope")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestFileBadID(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	bad := sample()
	bad.ID = "../escape"
	require.ErrorIs(t, st.Insert(ctx, bad), ErrBadID)
	_, err := st.Get(ctx, "a/b")
	require.ErrorIs(t, err, ErrBadID)
}

func TestFileListSortedByUpdatedDesc(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	a := sample()
	a.ID, a.TmuxSession, a.Ticket = "agent-aaaa", "agent-aaaa", ""
	b := sample()
	b.ID, b.TmuxSession, b.Ticket = "agent-bbbb", "agent-bbbb", ""
	require.NoError(t, st.Insert(ctx, a))
	require.NoError(t, st.Insert(ctx, b))
	// Touch a so its updated_at is newest.
	require.NoError(t, st.UpdateStatus(ctx, "agent-aaaa", StatusWorking))

	list, err := st.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, "agent-aaaa", list[0].ID, "most recently updated first")
}

func TestFileListSkipsCorruptFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := NewFileStore(dir)
	require.NoError(t, err)
	require.NoError(t, st.Insert(ctx, sample()))
	// Drop a junk .json file into sessions/.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sessions", "broken.json"), []byte("{not json"), 0o644))

	list, err := st.List(ctx)
	require.NoError(t, err, "one corrupt file must not fail List")
	require.Len(t, list, 1)
	require.Equal(t, "PROJ-350", list[0].ID)
}

func TestFileArchiveRemovesFromActive(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := NewFileStore(dir)
	require.NoError(t, err)
	require.NoError(t, st.Insert(ctx, sample()))

	require.NoError(t, st.Archive(ctx, "PROJ-350"))
	_, err = st.Get(ctx, "PROJ-350")
	require.ErrorIs(t, err, ErrNotFound, "archived session gone from active")
	require.FileExists(t, filepath.Join(dir, "closed", "PROJ-350.json"))
}

func TestFileDelete(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	require.NoError(t, st.Insert(ctx, sample()))
	require.NoError(t, st.Delete(ctx, "PROJ-350"))
	_, err := st.Get(ctx, "PROJ-350")
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, st.Delete(ctx, "PROJ-350"), ErrNotFound, "deleting missing is ErrNotFound")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/ -run TestFile`
Expected: FAIL — compile error `undefined: FileStore` / `undefined: NewFileStore` / `undefined: ErrBadID`.

- [ ] **Step 3: Write the core implementation**

Create `internal/store/file.go`:

```go
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrBadID is returned when a session id contains path separators or "..".
var ErrBadID = errors.New("invalid session id")

// FileStore persists each session as one pretty-printed JSON file under
// <dir>/sessions/<id>.json. Archived sessions move to <dir>/closed/<id>.json.
// The daemon is the only holder; an RWMutex serializes its concurrent callers
// (HTTP handlers, poller, classify goroutine). Writes are atomic (temp file +
// rename), so a crash never leaves a torn session file.
type FileStore struct {
	mu       sync.RWMutex
	dir      string
	sessions string
	closed   string
}

// NewFileStore creates the data dir layout and returns a ready store.
func NewFileStore(dir string) (*FileStore, error) {
	fs := &FileStore{
		dir:      dir,
		sessions: filepath.Join(dir, "sessions"),
		closed:   filepath.Join(dir, "closed"),
	}
	if err := os.MkdirAll(fs.sessions, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(fs.closed, 0o755); err != nil {
		return nil, err
	}
	return fs, nil
}

func safeID(id string) error {
	if id == "" || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return ErrBadID
	}
	return nil
}

func (fs *FileStore) activePath(id string) string { return filepath.Join(fs.sessions, id+".json") }
func (fs *FileStore) closedPath(id string) string { return filepath.Join(fs.closed, id+".json") }

// atomicWriteJSON marshals v and writes it to path via a temp file + rename, so
// readers never observe a partial file.
func atomicWriteJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// readSession loads and decodes a session file, mapping a missing file to
// ErrNotFound.
func readSession(path string) (*Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (fs *FileStore) Insert(ctx context.Context, s *Session) error {
	if err := safeID(s.ID); err != nil {
		return err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	path := fs.activePath(s.ID)
	if _, err := os.Stat(path); err == nil {
		return ErrExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	if s.Events == nil {
		s.Events = []Event{}
	}
	return atomicWriteJSON(path, s)
}

func (fs *FileStore) Get(ctx context.Context, id string) (*Session, error) {
	if err := safeID(id); err != nil {
		return nil, err
	}
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return readSession(fs.activePath(id))
}

func (fs *FileStore) List(ctx context.Context) ([]*Session, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	entries, err := os.ReadDir(fs.sessions)
	if err != nil {
		return nil, err
	}
	var out []*Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue // skips .tmp-* temp files too
		}
		s, err := readSession(filepath.Join(fs.sessions, e.Name()))
		if err != nil {
			log.Printf("filestore: skipping %s: %v", e.Name(), err)
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

func (fs *FileStore) Archive(ctx context.Context, id string) error {
	if err := safeID(id); err != nil {
		return err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	s, err := readSession(fs.activePath(id))
	if err != nil {
		return err
	}
	if err := atomicWriteJSON(fs.closedPath(id), s); err != nil {
		return err
	}
	return os.Remove(fs.activePath(id))
}

func (fs *FileStore) Delete(ctx context.Context, id string) error {
	if err := safeID(id); err != nil {
		return err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	err := os.Remove(fs.activePath(id))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

func (fs *FileStore) Ping(ctx context.Context) error {
	info, err := os.Stat(fs.sessions)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("filestore: %s is not a directory", fs.sessions)
	}
	return nil
}

func (fs *FileStore) Close(ctx context.Context) error { return nil }
```

(`Ping`/`Close` are defined now so the file compiles cleanly; the remaining mutators arrive in Task 2. `fmt`/`log` are already imported above.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ -run TestFile`
Expected: PASS (all `TestFile*` from Step 1).

- [ ] **Step 5: Commit**

```bash
git add internal/store/file.go internal/store/file_test.go
git commit -m "feat(store): FileStore core — insert/get/list/archive/delete with atomic JSON writes"
```

---

### Task 2: FileStore mutators — Update*/AppendEvent*/CAS + interface assertion + race test

**Files:**
- Modify: `internal/store/file.go`
- Test: `internal/store/file_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/file_test.go` (add `"sync"` and `"time"` to its import block):

```go
func TestFileUpdateStatusBumpsUpdatedAt(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	require.NoError(t, st.Insert(ctx, sample()))
	before, err := st.Get(ctx, "PROJ-350")
	require.NoError(t, err)

	time.Sleep(2 * time.Millisecond)
	require.NoError(t, st.UpdateStatus(ctx, "PROJ-350", StatusWorking))

	after, err := st.Get(ctx, "PROJ-350")
	require.NoError(t, err)
	require.Equal(t, StatusWorking, after.Status)
	require.True(t, after.UpdatedAt.After(before.UpdatedAt), "UpdateStatus must bump updated_at")
}

func TestFileUpdateStatusNotFound(t *testing.T) {
	require.ErrorIs(t, newFileStore(t).UpdateStatus(context.Background(), "nope", StatusWorking), ErrNotFound)
}

func TestFileUpdateStatusIf(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	require.NoError(t, st.Insert(ctx, sample())) // status = spawning

	// No-op when expected doesn't match.
	ok, err := st.UpdateStatusIf(ctx, "PROJ-350", StatusWorking, StatusIdle)
	require.NoError(t, err)
	require.False(t, ok)
	got, _ := st.Get(ctx, "PROJ-350")
	require.Equal(t, StatusSpawning, got.Status, "non-matching CAS leaves status unchanged")

	// Swaps when expected matches.
	ok, err = st.UpdateStatusIf(ctx, "PROJ-350", StatusSpawning, StatusWorking)
	require.NoError(t, err)
	require.True(t, ok)
	got, _ = st.Get(ctx, "PROJ-350")
	require.Equal(t, StatusWorking, got.Status)

	// Missing doc is (false, nil), not an error — matches Mongo's filtered update.
	ok, err = st.UpdateStatusIf(ctx, "ghost", StatusSpawning, StatusWorking)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestFileUpdateTypeAndSubjectAndPane(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	require.NoError(t, st.Insert(ctx, sample()))
	require.NoError(t, st.UpdateType(ctx, "PROJ-350", TypeDevelopment))
	require.NoError(t, st.UpdateSubject(ctx, "PROJ-350", "review auth module"))
	require.NoError(t, st.UpdatePane(ctx, "PROJ-350", "esc to interrupt"))

	got, err := st.Get(ctx, "PROJ-350")
	require.NoError(t, err)
	require.Equal(t, TypeDevelopment, got.Type)
	require.Equal(t, "review auth module", got.Subject)
	require.Equal(t, "esc to interrupt", got.LastPaneExcerpt)
}

func TestFileAppendEvent(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	require.NoError(t, st.Insert(ctx, sample()))
	require.NoError(t, st.AppendEvent(ctx, "PROJ-350", Event{Type: "Notification", Detail: "needs input"}))

	got, err := st.Get(ctx, "PROJ-350")
	require.NoError(t, err)
	require.Len(t, got.Events, 1)
	require.Equal(t, "Notification", got.Events[0].Type)
	require.False(t, got.Events[0].TS.IsZero(), "AppendEvent must stamp ts")
}

func TestFileAppendEventStatus(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	require.NoError(t, st.Insert(ctx, sample()))
	require.NoError(t, st.AppendEventStatus(ctx, "PROJ-350",
		Event{Type: "Stop"}, StatusIdle))

	got, err := st.Get(ctx, "PROJ-350")
	require.NoError(t, err)
	require.Len(t, got.Events, 1)
	require.Equal(t, StatusIdle, got.Status, "AppendEventStatus sets status atomically with the event")

	// Empty status only appends.
	require.NoError(t, st.AppendEventStatus(ctx, "PROJ-350", Event{Type: "Note"}, ""))
	got, _ = st.Get(ctx, "PROJ-350")
	require.Len(t, got.Events, 2)
	require.Equal(t, StatusIdle, got.Status, "empty status leaves status unchanged")
}

func TestFileConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	require.NoError(t, st.Insert(ctx, sample()))

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = st.UpdateStatus(ctx, "PROJ-350", StatusWorking)
			_ = st.AppendEvent(ctx, "PROJ-350", Event{Type: "tick"})
			_, _ = st.List(ctx)
		}()
	}
	wg.Wait()

	got, err := st.Get(ctx, "PROJ-350")
	require.NoError(t, err)
	require.Len(t, got.Events, 20, "every concurrent AppendEvent must be durable (no lost writes)")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/ -run TestFile`
Expected: FAIL — compile error: `st.UpdateStatus`/`UpdateStatusIf`/`UpdateType`/`UpdateSubject`/`UpdatePane`/`AppendEvent`/`AppendEventStatus` undefined on `*FileStore`.

- [ ] **Step 3: Add the mutators + interface assertion**

Append to `internal/store/file.go`:

```go
// mutate loads the active session, applies fn, bumps UpdatedAt, and writes it
// back atomically — the read-check-write runs under the write lock.
func (fs *FileStore) mutate(id string, fn func(*Session)) error {
	if err := safeID(id); err != nil {
		return err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	s, err := readSession(fs.activePath(id))
	if err != nil {
		return err
	}
	fn(s)
	s.UpdatedAt = time.Now().UTC()
	return atomicWriteJSON(fs.activePath(id), s)
}

func (fs *FileStore) UpdateStatus(ctx context.Context, id string, status Status) error {
	return fs.mutate(id, func(s *Session) { s.Status = status })
}

// UpdateStatusIf sets status to next only when the stored status still equals
// expected. A missing document returns (false, nil) — not an error — matching
// MongoStore's filtered update.
func (fs *FileStore) UpdateStatusIf(ctx context.Context, id string, expected, next Status) (bool, error) {
	if err := safeID(id); err != nil {
		return false, err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	s, err := readSession(fs.activePath(id))
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if s.Status != expected {
		return false, nil
	}
	s.Status = next
	s.UpdatedAt = time.Now().UTC()
	if err := atomicWriteJSON(fs.activePath(id), s); err != nil {
		return false, err
	}
	return true, nil
}

func (fs *FileStore) UpdateType(ctx context.Context, id string, t Type) error {
	return fs.mutate(id, func(s *Session) { s.Type = t })
}

func (fs *FileStore) UpdateSubject(ctx context.Context, id, subject string) error {
	return fs.mutate(id, func(s *Session) { s.Subject = subject })
}

func (fs *FileStore) UpdatePane(ctx context.Context, id, excerpt string) error {
	return fs.mutate(id, func(s *Session) { s.LastPaneExcerpt = excerpt })
}

func (fs *FileStore) AppendEvent(ctx context.Context, id string, ev Event) error {
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	return fs.mutate(id, func(s *Session) { s.Events = append(s.Events, ev) })
}

func (fs *FileStore) AppendEventStatus(ctx context.Context, id string, ev Event, status Status) error {
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	return fs.mutate(id, func(s *Session) {
		s.Events = append(s.Events, ev)
		if status != "" {
			s.Status = status
		}
	})
}

// Compile-time check that FileStore satisfies the full Store interface.
var _ Store = (*FileStore)(nil)
```

- [ ] **Step 4: Run tests to verify they pass (with the race detector)**

Run: `go test -race ./internal/store/ -run TestFile`
Expected: PASS — all `TestFile*`, no data races. `TestFileConcurrentAccess` confirms 20 events with no lost writes.

- [ ] **Step 5: Commit**

```bash
git add internal/store/file.go internal/store/file_test.go
git commit -m "feat(store): FileStore mutators, CAS, atomic event+status; satisfies Store"
```

---

### Task 3: De-Mongo `types.go` — drop bson tags, convert round-trip test to JSON

**Files:**
- Modify: `internal/store/types.go` (struct tags on `Event` and `Session`)
- Modify: `internal/store/types_test.go`

- [ ] **Step 1: Rewrite the round-trip test as JSON (failing first)**

Replace the entire body of `internal/store/types_test.go` with:

```go
package store

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionJSONRoundTrip(t *testing.T) {
	s := Session{
		ID:          "PROJ-350",
		Type:        TypeDevelopment,
		Ticket:      "PROJ-350",
		TmuxSession: "PROJ-350",
		Repo:        "/repo",
		Worktree:    ".worktrees/PROJ-350",
		Branch:      "PROJ-350",
		Prompt:      "do a security review of the auth module",
		Workdir:     "/Users/me/agentctl-agents/agent-a1b2",
		Subject:     "review auth module for security",
		Status:      StatusSpawning,
		PID:         123,
		Events:      []Event{{Type: "SessionStart"}},
	}
	raw, err := json.Marshal(s)
	require.NoError(t, err)

	var got Session
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, "PROJ-350", got.ID)
	require.Equal(t, TypeDevelopment, got.Type)
	require.Equal(t, StatusSpawning, got.Status)
	require.Len(t, got.Events, 1)
	require.Equal(t, "SessionStart", got.Events[0].Type)
	require.Equal(t, "do a security review of the auth module", got.Prompt)
	require.Equal(t, "/Users/me/agentctl-agents/agent-a1b2", got.Workdir)
	require.Equal(t, "review auth module for security", got.Subject)
}

func TestStatusValid(t *testing.T) {
	require.True(t, StatusWorking.Valid())
	require.False(t, Status("bogus").Valid())
}

func TestTypeNormalizeAndWorktreePolicy(t *testing.T) {
	// Known types keep their value; unknown collapses to "other".
	require.Equal(t, TypeDevelopment, NormalizeType("development"))
	require.Equal(t, TypeOther, NormalizeType("totally-made-up"))

	// Default worktree policy per design §2.
	require.True(t, TypeDevelopment.DefaultWorktree())
	require.True(t, TypePRReview.DefaultWorktree())
	require.False(t, TypeBuildkiteDebug.DefaultWorktree())
	require.False(t, TypeSpike.DefaultWorktree()) // opt-in via --worktree, not default
}
```

- [ ] **Step 2: Run the converted test to lock in green behavior (characterization)**

This is a refactor (removing dead bson tags), so the JSON round-trip test is a characterization test that must stay green before and after — there is no red phase. Run it now, against the unchanged struct, to confirm the JSON tags already drive correct round-tripping:
Run: `go test ./internal/store/ -run 'TestSessionJSONRoundTrip|TestStatusValid|TestTypeNormalizeAndWorktreePolicy'`
Expected: PASS. (If it fails to compile, it's because `mongo_test.go`'s `bson` import is gone — it isn't yet; that file is deleted in Task 5.)

- [ ] **Step 3: Remove the now-dead bson struct tags**

In `internal/store/types.go`, replace the `Event` and `Session` struct definitions with json-only tags (Mongo is gone, so bson tags are dead weight):

```go
type Event struct {
	TS     time.Time `json:"ts"`
	Type   string    `json:"type"`
	Detail string    `json:"detail"`
}

type Session struct {
	ID              string    `json:"id"`
	Type            Type      `json:"type"`
	Ticket          string    `json:"ticket"`       // optional
	TmuxSession     string    `json:"tmux_session"`
	Repo            string    `json:"repo"`
	Worktree        string    `json:"worktree"`     // optional (empty = no worktree)
	Branch          string    `json:"branch"`       // optional
	PR              string    `json:"pr"`           // optional (pr-review)
	Prompt          string    `json:"prompt"`       // initial prompt (prompt-spawned agents)
	Workdir         string    `json:"workdir"`      // absolute cwd of the tmux session
	Subject         string    `json:"subject"`      // one-line auto summary of what it's doing
	Status          Status    `json:"status"`
	PID             int       `json:"pid"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Events          []Event   `json:"events"`
	LastPaneExcerpt string    `json:"last_pane_excerpt"`
}
```

- [ ] **Step 4: Run the store tests**

Run: `go test ./internal/store/ -run 'TestSession|TestStatus|TestType|TestFile'`
Expected: PASS. (`mongo.go` still compiles — it references `bson.M`, not these struct tags.)

- [ ] **Step 5: Commit**

```bash
git add internal/store/types.go internal/store/types_test.go
git commit -m "refactor(store): drop dead bson struct tags; JSON round-trip test"
```

---

### Task 4: Switch config + daemon to FileStore (one commit, no broken build)

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/cli/daemon.go`

- [ ] **Step 1: Rewrite the config test for DataDir (failing first)**

Replace `TestLoadDefaults` and `TestLoadFromEnv` in `internal/config/config_test.go` and add data-dir tests. The full new file:

```go
package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("AGENTCTL_ADDR", "")
	c := Load()
	require.Equal(t, "127.0.0.1:8765", c.Addr)
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv("AGENTCTL_ADDR", "127.0.0.1:9000")
	require.Equal(t, "127.0.0.1:9000", Load().Addr)
}

func TestDataDirDefault(t *testing.T) {
	t.Setenv("AGENTCTL_DATA_DIR", "")
	c := Load()
	require.True(t, strings.HasSuffix(c.DataDir, ".agentctl"), "got %q", c.DataDir)
}

func TestDataDirFromEnv(t *testing.T) {
	t.Setenv("AGENTCTL_DATA_DIR", "/tmp/agentctl-data")
	require.Equal(t, "/tmp/agentctl-data", Load().DataDir)
}

func TestWorkdirDefault(t *testing.T) {
	t.Setenv("AGENTCTL_WORKDIR", "")
	c := Load()
	require.True(t, strings.HasSuffix(c.Workdir, "agentctl-agents"), "got %q", c.Workdir)
}

func TestWorkdirFromEnv(t *testing.T) {
	t.Setenv("AGENTCTL_WORKDIR", "/tmp/agents")
	require.Equal(t, "/tmp/agents", Load().Workdir)
}

func TestClaudeProjectsDirDefault(t *testing.T) {
	t.Setenv("CLAUDE_PROJECTS_DIR", "")
	c := Load()
	require.True(t, strings.HasSuffix(c.ClaudeProjectsDir, ".claude/projects"), "got %q", c.ClaudeProjectsDir)
}

func TestClaudeProjectsDirFromEnv(t *testing.T) {
	t.Setenv("CLAUDE_PROJECTS_DIR", "/tmp/projects")
	require.Equal(t, "/tmp/projects", Load().ClaudeProjectsDir)
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/config/`
Expected: FAIL — `c.DataDir` undefined.

- [ ] **Step 3: Update config.go**

In `internal/config/config.go`: drop `MongoURI`/`DB` from the struct, add `DataDir`; add a `defaultDataDir()` helper; replace the two Mongo lines in `Load()`:

Struct:
```go
type Config struct {
	Addr              string
	DataDir           string
	Workdir           string
	ClaudeProjectsDir string
}
```

Add helper (next to `defaultWorkdir`):
```go
func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".agentctl"
	}
	return filepath.Join(home, ".agentctl")
}
```

`Load()`:
```go
func Load() Config {
	return Config{
		Addr:              envOr("AGENTCTL_ADDR", "127.0.0.1:8765"),
		DataDir:           envOr("AGENTCTL_DATA_DIR", defaultDataDir()),
		Workdir:           envOr("AGENTCTL_WORKDIR", defaultWorkdir()),
		ClaudeProjectsDir: envOr("CLAUDE_PROJECTS_DIR", defaultClaudeProjectsDir()),
	}
}
```

- [ ] **Step 4: Rewire the daemon to FileStore**

In `internal/cli/daemon.go`, replace the store construction block:

```go
			st, err := store.NewFileStore(cfg.DataDir)
			if err != nil {
				return err
			}
			defer st.Close(context.Background())
```

(Replaces the `store.NewMongoStore(ctx, cfg.MongoURI, cfg.DB)` block. The `ctx`/`signal` setup, `MkdirAll(cfg.Workdir)`, and everything after are unchanged.)

Also update the command's `Short` description (it says "the single Mongo writer"):
```go
		Short: "Run the agentctl hub (HTTP API + poller; the single writer to the file store)",
```

- [ ] **Step 5: Build, vet, and test the whole module**

Run: `go build ./... && go vet ./... && go test ./internal/config/ ./internal/cli/ ./internal/daemon/`
Expected: PASS. (`mongo.go`/`mongo_test.go` still exist and compile; `NewMongoStore` is now unused but that's legal in Go. Mongo tests still run here only if you run `./internal/store/`.)

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go internal/cli/daemon.go
git commit -m "feat: wire daemon to FileStore; config DataDir replaces Mongo URI/DB"
```

---

### Task 5: Delete Mongo (code, compose, testcontainers) + tidy deps + Makefile

**Files:**
- Delete: `internal/store/mongo.go`
- Delete: `internal/store/mongo_test.go`
- Delete: `docker-compose.yml`
- Modify: `go.mod`, `go.sum` (via `go mod tidy`)
- Modify: `Makefile`

- [ ] **Step 1: Delete the Mongo files**

Run:
```bash
git rm internal/store/mongo.go internal/store/mongo_test.go docker-compose.yml
```

- [ ] **Step 2: Tidy the module (drops mongo-driver + testcontainers)**

Run: `go mod tidy`
Expected: `go.mod` no longer lists `go.mongodb.org/mongo-driver` or `github.com/testcontainers/testcontainers-go*`; `go.sum` shrinks.

- [ ] **Step 3: Update the Makefile — remove the Mongo targets**

In `Makefile`: drop `mongo-up` and `mongo-down` from the `.PHONY` line and delete both target blocks. New `.PHONY` line:
```make
.PHONY: build test lint run-daemon ui ui-dev web-test release install-skill
```
Delete:
```make
mongo-up:
	docker compose up -d mongo

mongo-down:
	docker compose down
```

- [ ] **Step 4: Verify the full build and test suite (no Docker)**

Run: `go build ./... && go vet ./... && go test -race ./...`
Expected: PASS for every package. The store package now runs only `file_test.go` + `types_test.go` — fast, no container. `git status` shows the two `.go` files and `docker-compose.yml` deleted.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "chore: remove MongoDB — drivers, compose, testcontainers, make targets"
```

---

### Task 6: Update docs + launchd plist for the file store

**Files:**
- Modify: `README.md`
- Modify: `docs/USAGE.md`
- Modify: `deploy/com.srajanpathak.agentctl.plist`

- [ ] **Step 1: README — replace Mongo/Docker references**

Make these edits in `README.md`:

- Line ~3 (intro): change the trailing "backed by a local daemon and MongoDB." → "backed by a local daemon and a file-based JSON store (no database to run)."
- Line ~5: change "`agentctl daemon` is the single MongoDB writer" → "`agentctl daemon` is the single writer to the on-disk session store".
- Prerequisites (line ~20): delete the "**Docker** — runs the local MongoDB instance via Docker Compose" bullet.
- Quick-start (line ~30): delete the `make mongo-up        # starts mongodb:7 on localhost:27017` line.
- Env-var table (lines ~93–94): replace the two Mongo rows with one:
  ```
  | `AGENTCTL_DATA_DIR` | `~/.agentctl` | Directory for session JSON files (`sessions/`, `closed/`) |
  ```
- launchd section (lines ~370–371): the "Start Mongo and the daemon" block — remove the `make mongo-up` line and reword the comment to "Start the daemon (once — then managed by launchd)".
- "make mongo-up / make mongo-down" reference (lines ~396–397): delete both lines.
- Testing note (lines ~401–404): replace the "Tests that need Docker (the Mongo integration suite) are skipped in `-short` mode" paragraph + the `go test -short ./...` snippet with:
  ```
  All tests run without Docker or any external services:

  ```bash
  go test ./...
  ```
  ```

- [ ] **Step 2: USAGE.md — replace Mongo/Docker references**

In `docs/USAGE.md`:
- Line ~22: change "Owns MongoDB, serves a loopback REST API…" → "Owns the on-disk session store, serves a loopback REST API…".
- Line ~49: delete the `docker ps            # MongoDB runs in a container` line (and its surrounding mention if it reads as a check step).
- Line ~72: delete `make mongo-up        # start MongoDB first`.
- Env-var table (lines ~369–370): replace the two Mongo rows with the single `AGENTCTL_DATA_DIR` row (same as README).
- Troubleshooting (line ~427): change "MongoDB down. `make mongo-up`, then check…" → "Data dir not writable. Check `AGENTCTL_DATA_DIR` (default `~/.agentctl`) and `/tmp/agentctl.daemon.err`."

- [ ] **Step 3: launchd plist — drop the Mongo env var**

In `deploy/com.srajanpathak.agentctl.plist`, delete the line:
```xml
    <key>AGENTCTL_MONGO_URI</key><string>mongodb://localhost:27017</string>
```
(The daemon defaults `DataDir` to `~/.agentctl`; no env entry is needed. If a non-default location is wanted later, add `<key>AGENTCTL_DATA_DIR</key><string>…</string>`.)

- [ ] **Step 4: Sanity-check no stale references remain**

Run: `grep -rn -i 'mongo\|mongodb\|27017\|docker compose\|mongo-up' README.md docs/USAGE.md deploy/ Makefile internal/ | grep -v -i 'mongo_test\|//'`
Expected: no output (no lingering Mongo/Docker references in code, docs, deploy, or Makefile).

- [ ] **Step 5: Commit**

```bash
git add README.md docs/USAGE.md deploy/com.srajanpathak.agentctl.plist
git commit -m "docs: file-based JSON store — drop Mongo/Docker from README, USAGE, plist"
```

---

## Final verification (after all tasks)

- [ ] `go build ./... && go vet ./... && go test -race ./...` — all green, no Docker required.
- [ ] `grep -rn -i 'mongo' --include='*.go' .` returns nothing.
- [ ] Manual smoke (optional): `make build && AGENTCTL_DATA_DIR=/tmp/ac-smoke ./bin/agentctl daemon` starts, `curl -s localhost:8765/healthz` returns ok, and `/tmp/ac-smoke/sessions/` + `/tmp/ac-smoke/closed/` exist.

Then proceed to **superpowers:finishing-a-development-branch**.
