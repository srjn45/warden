package store

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	scrivastore "github.com/srjn45/scriva/store"
)

// RecoveryReport describes records that can be reconstructed without mutating
// the source store. It is intentionally safe to JSON-encode for operator tools.
type RecoveryReport struct {
	Active                  []*Session              `json:"active"`
	Closed                  []*Session              `json:"closed"`
	Segments                int                     `json:"segments"`
	ValidEntries            int                     `json:"valid_entries"`
	Skipped                 []RecoveryIssue         `json:"skipped,omitempty"`
	LiveTmuxMissingMetadata []string                `json:"live_tmux_missing_metadata,omitempty"`
	Worktrees               []WorktreeRecoveryIssue `json:"worktrees,omitempty"`
	Reconciled              []string                `json:"reconciled,omitempty"`
}

type WorktreeRecoveryIssue struct {
	SessionID string `json:"session_id"`
	Path      string `json:"path"`
	Dirty     bool   `json:"dirty,omitempty"`
	Unpushed  bool   `json:"unpushed,omitempty"`
	Missing   bool   `json:"missing,omitempty"`
}

type RecoveryIssue struct {
	Collection string `json:"collection"`
	Segment    string `json:"segment"`
	Line       int    `json:"line"`
	Detail     string `json:"detail"`
}

// DiagnoseSessions reads Scriva segments directly and retains the newest valid
// operation for each logical session key. Bad framing, checksums and payloads
// are reported and skipped; the source directory is never opened writable.
func DiagnoseSessions(ctx context.Context, dataDir string) (*RecoveryReport, error) {
	report := &RecoveryReport{}
	for _, collection := range []string{"active", "closed"} {
		live, err := recoverCollection(ctx, dataDir, collection, report)
		if err != nil {
			return nil, err
		}
		for _, s := range live {
			if collection == "active" {
				report.Active = append(report.Active, s)
			} else {
				report.Closed = append(report.Closed, s)
			}
		}
	}
	sortSessions := func(ss []*Session) {
		sort.Slice(ss, func(i, j int) bool { return ss[i].UpdatedAt.After(ss[j].UpdatedAt) })
	}
	sortSessions(report.Active)
	sortSessions(report.Closed)
	closedByID := make(map[string]*Session, len(report.Closed))
	for _, s := range report.Closed {
		closedByID[s.ID] = s
	}
	active := report.Active[:0]
	for _, s := range report.Active {
		if closed := closedByID[s.ID]; closed != nil {
			report.Reconciled = append(report.Reconciled, s.ID)
			if !s.UpdatedAt.Before(closed.UpdatedAt) {
				delete(closedByID, s.ID)
				active = append(active, s)
			}
			continue
		}
		active = append(active, s)
	}
	report.Active = active
	closed := report.Closed[:0]
	for _, s := range report.Closed {
		if closedByID[s.ID] != nil {
			closed = append(closed, s)
		}
	}
	report.Closed = closed
	sort.Strings(report.Reconciled)
	return report, nil
}

func recoverCollection(ctx context.Context, dataDir, collection string, report *RecoveryReport) (map[string]*Session, error) {
	paths, err := filepath.Glob(filepath.Join(dataDir, "sessions-db", collection, "seg_*.ndjson"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	byEngineID := map[uint64]*Session{}
	for _, path := range paths {
		report.Segments++
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
		line := 0
		for scanner.Scan() {
			line++
			if err := ctx.Err(); err != nil {
				_ = f.Close()
				return nil, err
			}
			entry, err := scrivastore.Decode(scanner.Bytes())
			if err != nil {
				report.Skipped = append(report.Skipped, RecoveryIssue{collection, filepath.Base(path), line, err.Error()})
				continue
			}
			report.ValidEntries++
			if entry.Op == scrivastore.OpDelete {
				delete(byEngineID, entry.ID)
				continue
			}
			s, err := fromRecord(entry.Data)
			if err != nil || safeID(s.ID) != nil {
				if err == nil {
					err = safeID(s.ID)
				}
				report.Skipped = append(report.Skipped, RecoveryIssue{collection, filepath.Base(path), line, fmt.Sprintf("session payload: %v", err)})
				continue
			}
			byEngineID[entry.ID] = s
		}
		if err := scanner.Err(); err != nil {
			report.Skipped = append(report.Skipped, RecoveryIssue{collection, filepath.Base(path), line + 1, err.Error()})
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	}
	// Historical duplicate engine ids/keys can exist after corruption. Select the
	// newest valid session payload for each stable warden id.
	bySessionID := map[string]*Session{}
	for _, s := range byEngineID {
		if old := bySessionID[s.ID]; old == nil || s.UpdatedAt.After(old.UpdatedAt) {
			bySessionID[s.ID] = s
		}
	}
	return bySessionID, nil
}

// RebuildSessions writes recovered records to a fresh data directory.
func RebuildSessions(ctx context.Context, dst string, report *RecoveryReport) (retErr error) {
	if strings.TrimSpace(dst) == "" {
		return errors.New("repair destination is empty")
	}
	fs, err := NewFileStore(dst)
	if err != nil {
		return err
	}
	defer func() {
		if err := fs.Close(ctx); retErr == nil {
			retErr = err
		}
	}()
	for _, s := range report.Closed {
		copy := *s
		if err := fs.Insert(ctx, &copy); err != nil {
			return fmt.Errorf("rebuild closed %s: %w", s.ID, err)
		}
		if err := fs.Archive(ctx, s.ID); err != nil {
			return fmt.Errorf("archive rebuilt %s: %w", s.ID, err)
		}
	}
	for _, s := range report.Active {
		copy := *s
		if err := fs.Insert(ctx, &copy); err != nil {
			return fmt.Errorf("rebuild active %s: %w", s.ID, err)
		}
	}
	return nil
}

// WithOfflineSessionStore takes the same exclusive lock as FileStore without
// opening Scriva or modifying its segments. The lock is held for fn's duration.
func WithOfflineSessionStore(dataDir string, fn func() error) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	lock, err := acquireStoreLock(filepath.Join(dataDir, storeLockName))
	if err != nil {
		return err
	}
	defer lock.release()
	return fn()
}
