package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/store"
)

// pinnedWorkdir resolves the authoritative working dir for an agent action.
//
// When session resolves to a known agent with a Workdir:
//   - empty dir, or dir that matches the session Workdir → pin to sess.Workdir
//   - explicit dir that differs but belongs to the same git repository as
//     sess.Repo / sess.Workdir (e.g. a linked worktree) → honor dir
//   - explicit dir outside that repository → reject (do not silently fall back)
//
// Path identity uses symlink-canonicalized absolute paths so a symlink alias of
// the session worktree or of another same-repo worktree is accepted, while a
// symlink into an unrelated repository is rejected.
//
// An unknown session falls back to the supplied dir (a human may pass a stale
// id). status==0 means success; otherwise (status, msg) is the HTTP error the
// caller should write.
func (s *Server) pinnedWorkdir(ctx context.Context, session, dir string) (resolved string, sess *store.Session, status int, msg string) {
	resolved = dir
	if session != "" {
		got, err := s.store.Get(ctx, session)
		switch {
		case err == nil:
			sess = got
			if sess.Workdir != "" {
				if dir == "" || sameWorkdirPath(dir, sess.Workdir) {
					resolved = sess.Workdir
				} else {
					if err := ensureDirInSessionRepo(ctx, sess, dir); err != nil {
						return "", sess, errHTTPStatus(err), err.Error()
					}
					resolved = dir
				}
			}
		case errors.Is(err, store.ErrNotFound):
			// Unknown session: fall back to the provided dir.
		default:
			return "", nil, http.StatusInternalServerError, err.Error()
		}
	}
	if resolved == "" {
		return "", nil, http.StatusBadRequest, "no working directory: provide dir or a known session"
	}
	return resolved, sess, 0, ""
}

// dirOutsideRepoError is returned when an explicit dir is not part of the
// agent's repository. Callers map it to HTTP 403.
type dirOutsideRepoError struct {
	Dir string
}

func (e *dirOutsideRepoError) Error() string {
	return fmt.Sprintf("requested dir %q is outside the agent's repository/worktree", e.Dir)
}

func errHTTPStatus(err error) int {
	var outside *dirOutsideRepoError
	if errors.As(err, &outside) {
		return http.StatusForbidden
	}
	return http.StatusBadRequest
}

// ensureDirInSessionRepo verifies dir exists and shares the same git repository
// as the session (sess.Repo when set, otherwise sess.Workdir). Linked worktrees
// of the same repo are allowed; unrelated paths, bare repos, and .git dirs are
// rejected.
func ensureDirInSessionRepo(ctx context.Context, sess *store.Session, dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("requested dir %q is not a usable directory: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("requested dir %q is not a directory", dir)
	}
	if err := ensureGitWorktreeCheckout(ctx, dir); err != nil {
		return err
	}
	anchor := sess.Repo
	if anchor == "" {
		anchor = sess.Workdir
	}
	anchorCommon, err := gitCommonDir(ctx, anchor)
	if err != nil {
		return fmt.Errorf("resolve session repository for %q: %w", anchor, err)
	}
	dirCommon, err := gitCommonDir(ctx, dir)
	if err != nil {
		return fmt.Errorf("requested dir %q is not a git worktree: %w", dir, err)
	}
	if !sameWorkdirPath(anchorCommon, dirCommon) {
		return &dirOutsideRepoError{Dir: dir}
	}
	return nil
}

// ensureGitWorktreeCheckout rejects bare repositories and .git directories so
// commit/push/sync/check/snapshot only run against a real worktree checkout.
func ensureGitWorktreeCheckout(ctx context.Context, dir string) error {
	canon := canonicalizePath(dir)
	bare, err := gitRevParseBool(ctx, canon, "--is-bare-repository")
	if err != nil {
		return fmt.Errorf("requested dir %q is not a git worktree: %w", dir, err)
	}
	if bare {
		return fmt.Errorf("requested dir %q is a bare git repository, not a worktree", dir)
	}
	insideGit, err := gitRevParseBool(ctx, canon, "--is-inside-git-dir")
	if err != nil {
		return fmt.Errorf("requested dir %q is not a git worktree: %w", dir, err)
	}
	if insideGit {
		return fmt.Errorf("requested dir %q is a git dir (.git), not a worktree", dir)
	}
	insideWT, err := gitRevParseBool(ctx, canon, "--is-inside-work-tree")
	if err != nil {
		return fmt.Errorf("requested dir %q is not a git worktree: %w", dir, err)
	}
	if !insideWT {
		return fmt.Errorf("requested dir %q is not a git worktree", dir)
	}
	return nil
}

func gitRevParseBool(ctx context.Context, dir, flag string) (bool, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", flag).Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

// gitCommonDir returns the symlink-canonical absolute git common directory for
// dir (shared by all linked worktrees of a repository). Relative results from
// rev-parse are joined onto the canonical dir so comparisons are path-absolute
// and symlink-stable.
func gitCommonDir(ctx context.Context, dir string) (string, error) {
	canonDir := canonicalizePath(dir)
	out, err := exec.CommandContext(ctx, "git", "-C", canonDir, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return "", err
	}
	common := strings.TrimSpace(string(out))
	if common == "" {
		return "", fmt.Errorf("empty git-common-dir")
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(canonDir, common)
	}
	return canonicalizePath(common), nil
}

// canonicalizePath returns an absolute, cleaned path with symlinks evaluated
// when the path exists. Missing paths fall back to Abs/Clean so identity checks
// still work for synthetic session workdirs in tests.
func canonicalizePath(p string) string {
	clean := filepath.Clean(p)
	abs, err := filepath.Abs(clean)
	if err != nil {
		return clean
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// sameWorkdirPath reports whether a and b name the same filesystem path after
// absolute cleaning and symlink canonicalization (best-effort when a path is
// missing — then Abs/Clean identity is used).
func sameWorkdirPath(a, b string) bool {
	return canonicalizePath(a) == canonicalizePath(b)
}

// recordGitEvent appends a best-effort bookkeeping event linking a git action to
// the agent record. Failures are ignored — the git work already succeeded.
func (s *Server) recordGitEvent(id, kind, detail string) {
	_ = s.store.AppendEvent(context.Background(), id, store.Event{
		TS:     time.Now(),
		Type:   kind,
		Detail: detail,
	})
}
