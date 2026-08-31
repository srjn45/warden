package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/store"
)

func newRepairCmd() *cobra.Command {
	root := &cobra.Command{Use: "repair", Short: "Offline, backup-first repair tools"}
	root.AddCommand(newRepairSessionsCmd())
	return root
}

func newRepairSessionsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "sessions", Short: "Diagnose or reconstruct the offline session store", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := config.Load(configPathFor(cmd))
			apply, _ := cmd.Flags().GetBool("apply")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			jsonOut, _ := cmd.Flags().GetBool("json")
			backup, _ := cmd.Flags().GetString("backup")
			if apply && dryRun {
				return errors.New("--apply and --dry-run are mutually exclusive")
			}
			var report *store.RecoveryReport
			err := store.WithOfflineSessionStore(cfg.DataDir, func() error {
				var err error
				report, err = store.DiagnoseSessions(cmd.Context(), cfg.DataDir)
				return err
			})
			if err != nil {
				return fmt.Errorf("session store must be offline: %w", err)
			}
			enrichSessionReconciliation(report)
			if !apply {
				return printRecoveryReport(cmd.OutOrStdout(), report, jsonOut, true)
			}
			if backup == "" {
				return errors.New("repair sessions --apply requires --backup <path>")
			}
			if err := applySessionRepair(cmd, cfg.DataDir, backup, report); err != nil {
				return err
			}
			return printRecoveryReport(cmd.OutOrStdout(), report, jsonOut, false)
		}}
	cmd.Flags().Bool("apply", false, "apply the reconstruction (default is dry-run)")
	cmd.Flags().Bool("dry-run", false, "diagnose and report without changing session data")
	cmd.Flags().String("backup", "", "backup destination (required with --apply; must not exist)")
	cmd.Flags().Bool("json", false, "print the machine-readable recovery report")
	return cmd
}

func applySessionRepair(cmd *cobra.Command, dataDir, backup string, report *store.RecoveryReport) error {
	return store.WithOfflineSessionStore(dataDir, func() error {
		fresh, err := store.DiagnoseSessions(cmd.Context(), dataDir)
		if err != nil {
			return err
		}
		*report = *fresh
		if err := copyTree(dataDir, backup); err != nil {
			return fmt.Errorf("create backup: %w", err)
		}
		stage, err := os.MkdirTemp(filepath.Dir(dataDir), ".warden-session-repair-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(stage)
		if err := store.RebuildSessions(cmd.Context(), stage, report); err != nil {
			return err
		}
		verify, err := store.DiagnoseSessions(cmd.Context(), stage)
		if err != nil || len(verify.Skipped) != 0 || len(verify.Active) != len(report.Active) || len(verify.Closed) != len(report.Closed) {
			return fmt.Errorf("rebuilt store validation failed: err=%v skipped=%d", err, len(verify.Skipped))
		}
		if err := store.InstallRebuiltSessions(dataDir, filepath.Join(stage, "sessions-db")); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "backup: %s\nrollback: stop the daemon, then restore this backup over %s\n", backup, dataDir)
		return nil
	})
}

func copyTree(src, dst string) error {
	absSrc, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	absDst, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	if rel, err := filepath.Rel(absSrc, absDst); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("backup destination must be outside the data directory")
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("backup destination already exists")
	}
	type dirTime struct {
		path string
		when time.Time
	}
	var dirTimes []dirTime
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if err := os.Mkdir(target, info.Mode().Perm()); err != nil {
				return err
			}
			if err := os.Chmod(target, info.Mode().Perm()); err != nil {
				return err
			}
			dirTimes = append(dirTimes, dirTime{target, info.ModTime()})
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported backup file type at %s (%s)", path, info.Mode().Type())
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			_ = in.Close()
			return err
		}
		_, cpErr := io.Copy(out, in)
		inCloseErr := in.Close()
		syncErr := out.Sync()
		outCloseErr := out.Close()
		if cpErr != nil {
			return cpErr
		}
		if inCloseErr != nil {
			return inCloseErr
		}
		if syncErr != nil {
			return syncErr
		}
		if outCloseErr != nil {
			return outCloseErr
		}
		if err := os.Chmod(target, info.Mode().Perm()); err != nil {
			return err
		}
		return os.Chtimes(target, info.ModTime(), info.ModTime())
	})
	if err != nil {
		_ = os.RemoveAll(dst) // never leave a partial tree looking like a valid backup
		return err
	}
	// Creating children changes directory mtimes, so restore them bottom-up only
	// after the complete tree exists.
	for i := len(dirTimes) - 1; i >= 0; i-- {
		if err := os.Chtimes(dirTimes[i].path, dirTimes[i].when, dirTimes[i].when); err != nil {
			_ = os.RemoveAll(dst)
			return err
		}
	}
	return nil
}

func printRecoveryReport(w io.Writer, r *store.RecoveryReport, jsonOut, dry bool) error {
	if jsonOut {
		return json.NewEncoder(w).Encode(r)
	}
	mode := "repair complete"
	if dry {
		mode = "dry-run (no files changed)"
	}
	fmt.Fprintf(w, "session repair %s\nactive recovered: %d\nclosed recovered: %d\nsegments checked: %d\nvalid entries: %d\nskipped entries: %d\n", mode, len(r.Active), len(r.Closed), r.Segments, r.ValidEntries, len(r.Skipped))
	for _, issue := range r.Skipped {
		fmt.Fprintf(w, "  %s/%s line %d: %s\n", issue.Collection, issue.Segment, issue.Line, issue.Detail)
	}
	for _, name := range r.LiveTmuxMissingMetadata {
		fmt.Fprintf(w, "  live tmux missing metadata: %s (adopt explicitly if wanted)\n", name)
	}
	for _, wt := range r.Worktrees {
		fmt.Fprintf(w, "  worktree %s (%s): dirty=%t unpushed=%t missing=%t\n", wt.Path, wt.SessionID, wt.Dirty, wt.Unpushed, wt.Missing)
	}
	for _, id := range r.Reconciled {
		fmt.Fprintf(w, "  reconciled duplicate active/closed record: %s\n", id)
	}
	return nil
}

func enrichSessionReconciliation(r *store.RecoveryReport) {
	knownTmux := map[string]bool{}
	all := append(append([]*store.Session{}, r.Active...), r.Closed...)
	for _, s := range all {
		if s.TmuxSession != "" {
			knownTmux[s.TmuxSession] = true
		}
		path := s.Worktree
		if path != "" && !filepath.IsAbs(path) {
			path = filepath.Join(s.Repo, path)
		}
		if path == "" {
			continue
		}
		issue := store.WorktreeRecoveryIssue{SessionID: s.ID, Path: path}
		if _, err := os.Stat(path); err != nil {
			issue.Missing = true
		} else {
			if out, err := exec.Command("git", "-C", path, "status", "--porcelain").Output(); err == nil {
				issue.Dirty = len(out) > 0
			}
			if out, err := exec.Command("git", "-C", path, "rev-list", "@{upstream}..HEAD").Output(); err == nil {
				issue.Unpushed = strings.TrimSpace(string(out)) != ""
			}
		}
		if issue.Dirty || issue.Unpushed || issue.Missing {
			r.Worktrees = append(r.Worktrees, issue)
		}
	}
	if out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output(); err == nil {
		for _, name := range strings.Fields(string(out)) {
			if !knownTmux[name] {
				r.LiveTmuxMissingMetadata = append(r.LiveTmuxMissingMetadata, name)
			}
		}
	}
	sort.Strings(r.LiveTmuxMissingMetadata)
}
