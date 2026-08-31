package store

import (
	"bytes"
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

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
)

// ErrBadID is returned when a session id contains path separators or "..".
var ErrBadID = errors.New("invalid session id")

// ErrBadSessionRef is returned when a backend session reference (e.g. a
// claude/codex/opencode session id) carries characters outside the conservative
// allowlist. The reference is interpolated into a backend launch command, so
// rejecting shell-dangerous bytes here is defense-in-depth behind the backend's
// own shell-quoting.
var ErrBadSessionRef = errors.New("invalid backend session reference")

// sessionRefPattern is the conservative charset every real backend session id
// fits: UUIDs (claude), ses_… (opencode), 16-hex (crush), and similar. It
// deliberately excludes whitespace and shell metacharacters so an imported or
// adopted record cannot smuggle a command into a launch line.
var sessionRefPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// safeSessionRef validates a non-empty backend session reference. An empty
// reference is valid (non-pinning backends discover their own id post-launch).
func safeSessionRef(ref string) error {
	if ref == "" {
		return nil
	}
	if !sessionRefPattern.MatchString(ref) {
		return ErrBadSessionRef
	}
	return nil
}

// FileStore persists sessions in an embedded ScrivaDB (github.com/srjn45/scriva)
// rooted at <dir>/sessions-db/: live sessions in an "active" collection and
// archived ones in a "closed" collection, each record keyed by the session id.
// A write appends one record instead of rewriting a whole per-session JSON file
// (the write-amplification the previous FileStore carried). The collections are
// opened with SyncModeNone: like the previous implementation this is a localhost
// session store, so the last write surviving a power-loss is not a requirement
// (append-only segments rule out torn reads regardless).
//
// The daemon is the only holder. A single mutex serialises the compound
// read-modify-write methods (Insert's uniqueness scan + write, Update/mutate,
// UpdateStatusIf, FinalizeExit, Archive), mirroring mailbox; ScrivaDB does its own
// per-collection locking, so the store mutex only guards the read-then-write
// critical sections. Read-only methods take it too for a behaviour-identical
// faithful port of the previous RWMutex model.
type FileStore struct {
	mu     sync.Mutex
	db     *scriva.DB
	active *engine.Collection
	closed *engine.Collection
	lock   *storeLock // exclusive-writer ownership lock; released on Close
}

// storeLockName is the advisory ownership lock file in the data directory. It
// sits alongside sessions-db (NOT inside it, which the import path wipes) so the
// lock survives an import-wipe. Its presence alone means nothing — the OS-held
// flock on it is the authority (see storeLock).
const storeLockName = ".sessions-store.lock"

// importedMarker names the sentinel written (last) once the one-time legacy-JSON
// import into the ScrivaDB collections has completed. Its presence means the
// ScrivaDB is authoritative and no re-import runs; its absence means the import
// never finished, so the next open wipes the (derived) sessions-db and retries
// from the intact legacy JSON. See NewFileStore / importLegacy.
const importedMarker = ".sessions-filedb-imported"

// NewFileStore opens (creating if needed) the ScrivaDB-backed session store rooted
// at <dir>/sessions-db/ and, on first open, imports any legacy <dir>/sessions/
// and <dir>/closed/ JSON into it (subsuming the old provenance backfill). The
// import is guarded by importedMarker and is directory-atomic: if the sentinel
// is absent (never imported, or a prior attempt died partway) the derived
// sessions-db is wiped and rebuilt from the read-only legacy JSON, then the
// sentinel is written LAST — so a crash mid-import loses nothing.
func NewFileStore(dir string) (*FileStore, error) {
	dbDir := filepath.Join(dir, "sessions-db")
	sentinel := filepath.Join(dir, importedMarker)

	// Take the exclusive writer lock FIRST — before the import-wipe below can
	// RemoveAll the derived sessions-db — so exactly one process ever mutates the
	// live store. A second writable opener (a stray CLI, an offline repair run
	// while the daemon is up) is rejected here with ErrStoreOwned rather than
	// racing writes into the shared append-only segments. The data dir must exist
	// to hold the lock file; create it before locking (never the db dir, which the
	// import path may wipe).
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := acquireStoreLock(filepath.Join(dir, storeLockName))
	if err != nil {
		return nil, err
	}

	imported, err := fileExists(sentinel)
	if err != nil {
		_ = lock.release()
		return nil, err
	}
	if !imported {
		// Wipe any partial/failed prior attempt so the import starts from a clean
		// slate (a half-loaded collection would abort LoadJSONL on ErrDuplicateKey).
		// Safe: sessions-db holds nothing not reproducible from the legacy JSON
		// until the sentinel says the import finished.
		if err := os.RemoveAll(dbDir); err != nil {
			_ = lock.release()
			return nil, err
		}
	}
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		_ = lock.release()
		return nil, err
	}

	db, err := scriva.Open(dbDir, scriva.WithSyncMode(engine.SyncModeNone))
	if err != nil {
		_ = lock.release()
		return nil, err
	}
	active, err := db.Collection("active")
	if err != nil {
		db.Close()
		_ = lock.release()
		return nil, err
	}
	closed, err := db.Collection("closed")
	if err != nil {
		db.Close()
		_ = lock.release()
		return nil, err
	}
	fs := &FileStore{db: db, active: active, closed: closed, lock: lock}

	if !imported {
		if err := importLegacy(dir, active, closed); err != nil {
			db.Close()
			_ = lock.release()
			return nil, err
		}
		// Sentinel LAST: only now is the ScrivaDB authoritative.
		if err := os.WriteFile(sentinel, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
			db.Close()
			_ = lock.release()
			return nil, err
		}
	}
	return fs, nil
}

// fileExists reports whether path exists, distinguishing a genuine stat error
// from a plain not-exist.
func fileExists(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else {
		return false, err
	}
}

// provenanceMarker names the sentinel the OLD FileStore wrote once its one-shot
// provenance backfill had run. It is now read-only INPUT to the importer: if it
// is present the legacy JSON already carries explicit provenance flags (import
// them verbatim, no re-backfill); if absent, the importer backfills during
// import. The old marker is never written anymore and can be left on disk.
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

// importLegacy performs the one-time import of the legacy per-file JSON into the
// ScrivaDB collections, folding the old provenance backfill into the same pass.
// Each legacy dir is decoded file-by-file (skip+warn on corrupt/unsafe-id,
// matching the old listDir), then loaded into its collection with LoadJSONL,
// which is atomic per collection (all-or-nothing). A missing legacy dir (fresh
// install) is simply an empty import.
func importLegacy(dir string, active, closed *engine.Collection) error {
	// Did the old code already backfill explicit provenance flags into the legacy
	// JSON? If so, import them verbatim so an adopted (WorktreeCreated=false)
	// record is never clobbered; otherwise infer them now.
	provDone, err := fileExists(filepath.Join(dir, provenanceMarker))
	if err != nil {
		return err
	}
	srcs := []struct {
		dir string
		col *engine.Collection
	}{
		{filepath.Join(dir, "sessions"), active},
		{filepath.Join(dir, "closed"), closed},
	}
	for _, src := range srcs {
		buf, err := legacyNDJSON(src.dir, provDone)
		if err != nil {
			return err
		}
		if buf.Len() == 0 {
			continue // no legacy dir, or no readable records
		}
		if _, err := src.col.LoadJSONL(&buf, "id"); err != nil {
			return err
		}
	}
	return nil
}

// legacyNDJSON decodes every *.json in srcDir individually and returns the good
// records as an NDJSON buffer (one Session per line, keyed by "id"). Corrupt or
// unsafe-id files are skipped with a warning — a bad file never blocks the
// upgrade — so the batch handed to LoadJSONL is always parseable and its
// all-or-nothing guarantee then protects only against a write-side failure. When
// provDone is false the provenance flags are backfilled during this pass.
func legacyNDJSON(srcDir string, provDone bool) (bytes.Buffer, error) {
	var buf bytes.Buffer
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return buf, nil // no legacy dir → nothing to import
		}
		return buf, err
	}
	enc := json.NewEncoder(&buf) // Encode writes one compact JSON line + newline
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue // skips .tmp-* temp files too
		}
		s, err := readSession(filepath.Join(srcDir, e.Name()))
		if err != nil {
			slog.Warn("filestore: import skipping unreadable legacy session", "file", e.Name(), "err", err)
			continue
		}
		if err := safeID(s.ID); err != nil {
			slog.Warn("filestore: import skipping legacy session with unsafe id", "file", e.Name(), "id", s.ID)
			continue
		}
		if !provDone {
			backfillProvenance(s)
		}
		if err := enc.Encode(s); err != nil {
			return buf, err
		}
	}
	return buf, nil
}

func safeID(id string) error {
	// "/" and "\" plus ".." guard against path traversal (the id was a filename
	// component historically and is a tmux target now); ":" is a tmux target
	// separator (session:window) that would silently break `tmux -t <id>`.
	if id == "" || strings.ContainsAny(id, `/\:`) || strings.Contains(id, "..") {
		return ErrBadID
	}
	return nil
}

// SafeID reports whether id is a valid session id (no path separators or "..").
// Exported for callers that validate a candidate id before insert (e.g. adopt).
func SafeID(id string) error { return safeID(id) }

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
// readers never observe a partial file. Retained for tests that seed legacy JSON
// files; the store's own writes go through the ScrivaDB collections.
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
// ErrNotFound. It backs the legacy-JSON importer and is fuzz-tested.
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

// toRecord decomposes a Session into a ScrivaDB record body via a JSON round-trip,
// so its fields stay real in the store (indexable in future) rather than an
// opaque blob. Always round-trip through JSON — never read typed business logic
// off the raw map — because a map[string]any returns numbers as float64, times
// as strings, etc.; the JSON round-trip through Session's own tags is lossless.
// The engine stamps the reserved _key on InsertWithKey/Upsert/UpdateByKey, so it
// must NOT be present here.
func toRecord(s *Session) (map[string]any, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// fromRecord reconstructs a Session from a record body. The reserved _key the
// engine stamped into the map is harmlessly dropped on unmarshal (Session has no
// _key json tag).
func fromRecord(d map[string]any) (*Session, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// getFrom reads the session keyed id from col, mapping a key miss to ErrNotFound.
func getFrom(col *engine.Collection, id string) (*Session, error) {
	r, err := col.GetByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return fromRecord(r.Data)
}

// scanTolerant returns every decodable session in col, newest-updated first,
// TOLERANTLY: an undecodable record is skipped with a warning rather than
// failing the whole scan (matching the old listDir's corrupt-file tolerance).
// This is the ARCHIVE (closed-collection) policy — a single unreadable
// historical record must not make the whole history unlistable. It returns the
// count of records that had to be skipped so a caller can flag a history export
// as degraded (never presenting a short export as a complete backup). Any
// engine-level scan error (segment/checksum/index) is returned as-is. The active
// collection uses scanActiveStrict instead.
func scanTolerant(col *engine.Collection, colName string) ([]*Session, int, error) {
	results, err := col.Scan(query.MatchAll)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*Session, 0, len(results))
	skipped := 0
	for _, r := range results {
		s, err := fromRecord(r.Data)
		if err != nil {
			key, _ := r.Data[engine.KeyField].(string)
			slog.Warn("filestore: skipping undecodable session record", "collection", colName, "key", key, "err", err)
			skipped++
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, skipped, nil
}

// scanActiveStrict returns every session in the ACTIVE collection newest-updated
// first, or a typed error — never a silent subset. The active fleet is the
// authoritative live view (TUI/REST/SSE poll it every second), so an incomplete
// scan must surface as an explicit degradation, not a shorter list:
//
//   - an engine-level scan failure (segment framing, checksum, index read) is
//     wrapped as a DegradedScanError with class "read";
//   - any record that decodes at the engine layer but not into a Session is
//     collected as a class "decode" failure, and if ANY such failure occurs the
//     whole scan returns a DegradedScanError carrying every failure's diagnostics.
//
// This is the Phase-3 complete-or-error invariant: no nil-error partial list
// crosses the Store boundary for the active collection.
func scanActiveStrict(col *engine.Collection) ([]*Session, error) {
	results, err := col.Scan(query.MatchAll)
	if err != nil {
		return nil, &DegradedScanError{Failures: []ScanFailure{{
			Collection: "active", Class: DegradeRead, Detail: err.Error(),
		}}}
	}
	out := make([]*Session, 0, len(results))
	var failures []ScanFailure
	for _, r := range results {
		s, derr := fromRecord(r.Data)
		if derr != nil {
			key, _ := r.Data[engine.KeyField].(string)
			failures = append(failures, ScanFailure{
				Collection: "active", Key: key, Class: DegradeDecode, Detail: derr.Error(),
			})
			continue
		}
		out = append(out, s)
	}
	if len(failures) > 0 {
		return nil, &DegradedScanError{Failures: failures}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// writeActive persists s back into the active collection (whole-record atomic
// update). The caller must hold fs.mu and have confirmed the record exists.
func (fs *FileStore) writeActive(s *Session) error {
	rec, err := toRecord(s)
	if err != nil {
		return err
	}
	_, err = fs.active.UpdateByKey(s.ID, rec)
	return err
}

func (fs *FileStore) Insert(ctx context.Context, s *Session) error {
	// Validate name format
	if err := ValidateName(s.Name); err != nil {
		return err
	}

	if err := safeID(s.ID); err != nil {
		return err
	}

	if err := safeSessionRef(s.ClaudeSessionID); err != nil {
		return err
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Check name uniqueness INSIDE lock to prevent race condition. It stays an
	// O(n) active scan (as the old listLocked loop); a name unique-index is a
	// documented future optimization.
	if s.Name != "" {
		sessions, err := scanActiveStrict(fs.active)
		if err != nil {
			return err
		}
		for _, existing := range sessions {
			if existing.Name == s.Name {
				return ErrNameExists
			}
		}
	}

	exists, err := fs.active.Exists(s.ID)
	if err != nil {
		return err
	}
	if exists {
		return ErrExists
	}
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	if s.Events == nil {
		s.Events = []Event{}
	}
	rec, err := toRecord(s)
	if err != nil {
		return err
	}
	if _, _, err := fs.active.InsertWithKey(s.ID, rec); err != nil {
		if errors.Is(err, engine.ErrDuplicateKey) {
			return ErrExists
		}
		return err
	}
	return nil
}

func (fs *FileStore) Get(ctx context.Context, id string) (*Session, error) {
	if err := safeID(id); err != nil {
		return nil, err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return getFrom(fs.active, id)
}

func (fs *FileStore) GetByNameOrID(ctx context.Context, nameOrID string) (*Session, error) {
	if err := safeID(nameOrID); err != nil {
		return nil, err
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// First pass: scan for exact name match
	sessions, err := scanActiveStrict(fs.active)
	if err != nil {
		return nil, err
	}
	for _, s := range sessions {
		if s.Name != "" && s.Name == nameOrID {
			return s, nil
		}
	}

	// Second pass: fall back to ID lookup
	return getFrom(fs.active, nameOrID)
}

// List returns the active fleet complete-or-error: a degraded active scan yields
// a *DegradedScanError (never a silent subset), so REST/SSE can degrade
// explicitly. See scanActiveStrict.
func (fs *FileStore) List(ctx context.Context) ([]*Session, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return scanActiveStrict(fs.active)
}

// ListClosed returns all archived (closed) sessions, newest-updated first,
// tolerantly: a single unreadable historical record is skipped (and logged)
// rather than making the whole history unlistable. Use ListClosedDegraded when
// the skipped-record count matters (e.g. flagging a history export as degraded).
func (fs *FileStore) ListClosed(ctx context.Context) ([]*Session, error) {
	sessions, _, err := fs.ListClosedDegraded(ctx)
	return sessions, err
}

// ListClosedDegraded is ListClosed plus the count of archive records that had to
// be skipped because they would not decode. A non-zero count means the returned
// history is incomplete — callers exporting a backup must surface that rather
// than present a short export as complete.
func (fs *FileStore) ListClosedDegraded(ctx context.Context) ([]*Session, int, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return scanTolerant(fs.closed, "closed")
}

// Update is the general transactional read-modify-write primitive (see the Store
// interface doc): it loads the active session, applies fn, and — unless fn
// returns an error, which aborts leaving the stored session untouched — bumps
// UpdatedAt and writes it back atomically under the store lock. mutate and the
// remaining narrow setters funnel through here, so there is one CAS-safe
// read-modify-write path.
func (fs *FileStore) Update(ctx context.Context, id string, fn func(s *Session) error) error {
	if err := safeID(id); err != nil {
		return err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	s, err := getFrom(fs.active, id)
	if err != nil {
		return err
	}
	if err := fn(s); err != nil {
		return err
	}
	s.UpdatedAt = time.Now().UTC()
	return fs.writeActive(s)
}

// mutate is the infallible-fn convenience over Update, for the narrow setters
// that can never fail their mutation. It runs the read-check-write under the
// store lock.
func (fs *FileStore) mutate(id string, fn func(*Session)) error {
	return fs.Update(context.Background(), id, func(s *Session) error {
		fn(s)
		return nil
	})
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
	s, err := getFrom(fs.active, id)
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
	if err := fs.writeActive(s); err != nil {
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
	s, err := getFrom(fs.active, id)
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
	if err := fs.writeActive(s); err != nil {
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

func (fs *FileStore) SetSessionID(ctx context.Context, id, sessionID string) error {
	// The pinned id is discovered by parsing a backend's own transcript/output and
	// later interpolated into a resume command, so validate it here too — the same
	// defense-in-depth guard Insert applies.
	if err := safeSessionRef(sessionID); err != nil {
		return err
	}
	return fs.mutate(id, func(s *Session) { s.ClaudeSessionID = sessionID })
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

// SetForceCompact sets the per-agent force-compact override. v=nil clears the
// override so the agent inherits the global token_force_compact setting; a
// non-nil pointer pins the agent on (true) or off (false) regardless of global.
func (fs *FileStore) SetForceCompact(ctx context.Context, id string, v *bool) error {
	return fs.mutate(id, func(s *Session) {
		s.ForceCompact = v
	})
}

func (fs *FileStore) UpdatePermissionMode(ctx context.Context, id string, mode string) error {
	return fs.mutate(id, func(s *Session) {
		s.PermissionMode = mode
	})
}

// UpdateRole sets the built-in role for a session. Empty means the "general"
// role (no persona). The role name (not the persona text) is what's persisted;
// the persona is re-resolved from the registry at every (re)launch.
func (fs *FileStore) UpdateRole(ctx context.Context, id string, role string) error {
	return fs.mutate(id, func(s *Session) {
		s.Role = role
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

// Archive moves the session to the closed collection (soft delete). It writes the
// closed copy first and removes the active record second, so a crash between the
// two leaves the session recoverable in active, never lost (at worst it appears
// in both). An existing closed record for the same id is overwritten (Upsert).
func (fs *FileStore) Archive(ctx context.Context, id string) error {
	if err := safeID(id); err != nil {
		return err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	s, err := getFrom(fs.active, id)
	if err != nil {
		return err
	}
	rec, err := toRecord(s)
	if err != nil {
		return err
	}
	if _, err := fs.closed.Upsert(id, rec); err != nil {
		return err
	}
	if err := fs.active.DeleteByKey(id); err != nil && !errors.Is(err, engine.ErrKeyNotFound) {
		return err
	}
	return nil
}

func (fs *FileStore) Delete(ctx context.Context, id string) error {
	if err := safeID(id); err != nil {
		return err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	err := fs.active.DeleteByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return ErrNotFound
	}
	return err
}

func (fs *FileStore) Ping(ctx context.Context) error {
	// A cheap liveness check on the active collection (its index read) stands in
	// for the old sessions/ dir-stat.
	_, err := fs.active.Count(query.MatchAll)
	return err
}

// Close flushes the ScrivaDB index, stops its background compaction goroutine,
// and releases the exclusive-writer ownership lock LAST — so the lock is held
// for the full lifetime of the open store and only drops once the db is safely
// closed, letting offline maintenance take over cleanly. The daemon defers it on
// shutdown. (The old FileStore.Close was a no-op.)
func (fs *FileStore) Close(ctx context.Context) error {
	err := fs.db.Close()
	if lerr := fs.lock.release(); err == nil {
		err = lerr
	}
	return err
}
