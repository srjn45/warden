package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
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
// (HTTP handlers, poller, classify goroutine). Writes go through a temp file +
// rename, so a concurrent reader never observes a partially written file. (This
// guards torn reads, not power-loss durability — the store is not fsync'd; for a
// localhost session store the last write surviving a crash is not a requirement.)
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
	if err := os.MkdirAll(fs.sessions, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(fs.closed, 0o700); err != nil {
		return nil, err
	}
	if err := fs.migrateProvenance(); err != nil {
		return nil, err
	}
	return fs, nil
}

// provenanceMarker names the sentinel that records the one-shot provenance
// backfill has run, so it never re-touches records written after the migration.
const provenanceMarker = ".provenance-migrated"

// backfillProvenance infers created/adopted provenance for a legacy record that
// predates the WorktreeCreated/BranchCreated fields. A worktree on disk implies
// warden created it (adopted worktrees did not exist before this feature), and a
// recorded branch equal to the session id is warden's default branch name, so it
// is treated as warden-created; a user-named branch (≠ id) is conservatively
// left adopted.
func backfillProvenance(s *Session) {
	s.WorktreeCreated = s.Worktree != ""
	s.BranchCreated = s.Branch != "" && s.Branch == s.ID
}

// migrateProvenance backfills the provenance flags onto every pre-existing
// active and archived record exactly once (guarded by a sentinel file), then
// rewrites them. Running before the daemon serves any request, it needs no lock.
// Records written after the marker exists keep the explicit flags spawn records,
// so a legitimately-adopted (WorktreeCreated=false) record is never clobbered.
func (fs *FileStore) migrateProvenance() error {
	marker := filepath.Join(fs.dir, provenanceMarker)
	if _, err := os.Stat(marker); err == nil {
		return nil // already migrated
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, dir := range []string{fs.sessions, fs.closed} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			s, err := readSession(path)
			if err != nil {
				slog.Warn("filestore: provenance migration skipping unreadable session", "file", e.Name(), "err", err)
				continue
			}
			backfillProvenance(s)
			if err := atomicWriteJSON(path, s); err != nil {
				return err
			}
		}
	}
	return os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

func safeID(id string) error {
	// "/" and "\" plus ".." guard against path traversal (the id is a filename
	// component); ":" is a tmux target separator (session:window) that would
	// silently break `tmux -t <id>` targeting.
	if id == "" || strings.ContainsAny(id, `/\:`) || strings.Contains(id, "..") {
		return ErrBadID
	}
	return nil
}

// SafeID reports whether id is a valid session id (no path separators or "..").
// Exported for callers that validate a candidate id before insert (e.g. adopt).
func SafeID(id string) error { return safeID(id) }

func (fs *FileStore) activePath(id string) string { return filepath.Join(fs.sessions, id+".json") }
func (fs *FileStore) closedPath(id string) string { return filepath.Join(fs.closed, id+".json") }

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

// ValidateName checks that name matches the allowed format (alphanumeric + hyphens/underscores, 1-32 chars).
// Empty names are valid (no-name agents).
func ValidateName(name string) error {
	if name == "" {
		return nil // empty is valid
	}
	if !namePattern.MatchString(name) {
		return ErrInvalidName
	}
	return nil
}

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

// listDir reads every session JSON in dir, newest-updated first. It is the
// shared body of List (active) and ListClosed (archived).
func listDir(dir string) ([]*Session, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue // skips .tmp-* temp files too
		}
		s, err := readSession(filepath.Join(dir, e.Name()))
		if err != nil {
			slog.Warn("filestore: skipping unreadable session", "file", e.Name(), "err", err)
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// listLocked reads all active sessions without acquiring a lock. The caller must
// hold fs.mu (read or write).
func (fs *FileStore) listLocked(ctx context.Context) ([]*Session, error) {
	return listDir(fs.sessions)
}

func (fs *FileStore) Insert(ctx context.Context, s *Session) error {
	// Validate name format
	if err := ValidateName(s.Name); err != nil {
		return err
	}

	if err := safeID(s.ID); err != nil {
		return err
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Check name uniqueness INSIDE lock to prevent race condition
	if s.Name != "" {
		sessions, err := fs.listLocked(ctx)
		if err != nil {
			return err
		}
		for _, existing := range sessions {
			if existing.Name == s.Name {
				return ErrNameExists
			}
		}
	}

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

func (fs *FileStore) GetByNameOrID(ctx context.Context, nameOrID string) (*Session, error) {
	if err := safeID(nameOrID); err != nil {
		return nil, err
	}

	fs.mu.RLock()
	defer fs.mu.RUnlock()

	// First pass: scan for exact name match
	sessions, err := fs.listLocked(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range sessions {
		if s.Name != "" && s.Name == nameOrID {
			return s, nil
		}
	}

	// Second pass: fall back to ID lookup
	return readSession(fs.activePath(nameOrID))
}

func (fs *FileStore) List(ctx context.Context) ([]*Session, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.listLocked(ctx)
}

// ListClosed returns all archived (closed) sessions, newest-updated first.
func (fs *FileStore) ListClosed(ctx context.Context) ([]*Session, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return listDir(fs.closed)
}

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
// expected. A missing document returns (false, nil) — not an error — so a
// compare-and-swap against an archived/deleted session is a no-op, not a failure.
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

// FinalizeExit sets status next (CAS on expected), records ExitCode=code, and
// for a non-zero code appends a "session exited" event — in one atomic write.
func (fs *FileStore) FinalizeExit(ctx context.Context, id string, expected, next Status, code int) (bool, error) {
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
	c := code
	s.ExitCode = &c
	now := time.Now().UTC()
	if code != 0 {
		s.Events = append(s.Events, Event{
			TS:     now,
			Type:   "exit",
			Detail: exitDetail(code),
		})
	}
	s.UpdatedAt = now
	if err := atomicWriteJSON(fs.activePath(id), s); err != nil {
		return false, err
	}
	return true, nil
}

// exitDetail renders a human-readable exit reason. A code in the shell's
// "killed by signal" range (128 < code <= 128+64) names the signal; an unknown
// signal number in that range falls through to the plain "code N" format.
func exitDetail(code int) string {
	if sig := signalName(code - 128); code > 128 && code <= 128+64 && sig != "" {
		return fmt.Sprintf("session exited: code %d (%s)", code, sig)
	}
	return fmt.Sprintf("session exited: code %d", code)
}

// signalName maps the common termination signals to their names; "" for others.
func signalName(sig int) string {
	switch sig {
	case 2:
		return "SIGINT"
	case 6:
		return "SIGABRT"
	case 9:
		return "SIGKILL"
	case 11:
		return "SIGSEGV"
	case 15:
		return "SIGTERM"
	}
	return ""
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

func (fs *FileStore) SetRestart(ctx context.Context, id string, count int, at time.Time) error {
	return fs.mutate(id, func(s *Session) { s.RestartCount = count; t := at; s.LastRestartAt = &t })
}

// UpdateContext persists an agent's context-window gauge. It stamps
// ContextCheckedAt, and appends a single "context" event ONLY when the state
// band actually changes (e.g. ok→warning), so steady-state refreshes don't grow
// the event log.
func (fs *FileStore) UpdateContext(ctx context.Context, id string, tokens int, state string) error {
	return fs.mutate(id, func(s *Session) {
		if state != "" && state != s.ContextState {
			s.Events = append(s.Events, Event{
				TS:     time.Now().UTC(),
				Type:   "context",
				Detail: fmt.Sprintf("context %s→%s (%dk)", orNone(s.ContextState), state, tokens/1000),
			})
		}
		s.ContextTokens = tokens
		s.ContextState = state
		s.ContextCheckedAt = time.Now().UTC()
	})
}

// orNone renders an empty prior state as "none" for the transition event.
func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// StampCompact records that warden auto-sent /compact to id just now (cooldown
// guard for the context-size guard).
func (fs *FileStore) StampCompact(ctx context.Context, id string) error {
	return fs.mutate(id, func(s *Session) {
		now := time.Now().UTC()
		s.LastCompactAt = &now
	})
}

func (fs *FileStore) UpdateAutoApprove(ctx context.Context, id string, enabled bool) error {
	return fs.mutate(id, func(s *Session) {
		s.AutoApprove = enabled
	})
}

func (fs *FileStore) UpdatePermissionMode(ctx context.Context, id string, mode string) error {
	return fs.mutate(id, func(s *Session) {
		s.PermissionMode = mode
	})
}

func (fs *FileStore) ClearWorktree(ctx context.Context, id string) error {
	return fs.mutate(id, func(s *Session) { s.Worktree = ""; s.Branch = "" })
}

func (fs *FileStore) SetRateLimit(ctx context.Context, id string, restoreAt time.Time, retryCount int) error {
	return fs.mutate(id, func(sess *Session) {
		now := time.Now().UTC()

		// Preserve first RateLimitedAt time
		if sess.RateLimitedAt == nil {
			sess.RateLimitedAt = &now
		}

		sess.RateLimitRestoreAt = &restoreAt
		sess.RateLimitRetryCount = retryCount

		// Append event for tracking
		sess.Events = append(sess.Events, Event{
			TS:   now,
			Type: "rate-limit",
			Detail: fmt.Sprintf("scheduled resume at %s (retry %d)",
				restoreAt.Format(time.RFC3339), retryCount),
		})
	})
}

func (fs *FileStore) ClearRateLimit(ctx context.Context, id string) error {
	return fs.mutate(id, func(sess *Session) {
		sess.RateLimitedAt = nil
		sess.RateLimitRestoreAt = nil
		sess.RateLimitRetryCount = 0

		sess.Events = append(sess.Events, Event{
			TS:     time.Now().UTC(),
			Type:   "rate-limit-resumed",
			Detail: "successfully resumed after rate limit",
		})
	})
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

// Archive moves the session to closed/<id>.json (soft delete). It writes the
// closed copy first and removes the active file second, so a crash between the
// two leaves the session recoverable in active, never lost (at worst it appears
// in both). An existing closed/<id>.json for the same id is overwritten.
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
