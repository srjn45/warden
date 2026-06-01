package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/srajanpathak/agentctl/internal/store"
)

// claudeCmd is launched in every spawned session. Agents run unattended
// (design §4): permission prompts are skipped; the Notification hook still
// records when one *would* have prompted.
const claudeCmd = "claude --dangerously-skip-permissions"

type Lifecycle struct {
	run Runner
}

func New(r Runner) *Lifecycle { return &Lifecycle{run: r} }

// SpawnRequest is the type-aware input to Spawn (design §2 / §6).
type SpawnRequest struct {
	Type     store.Type
	Ticket   string // optional; becomes the id when present
	Repo     string
	Branch   string // optional; development branch / pr-review checkout target
	PR       string // optional; pr-review
	Worktree bool   // analysis/spike opt-in
}

func worktreeRel(id string) string { return filepath.Join(".worktrees", id) }

// shortID returns 4 hex chars for auto-generated session ids.
func shortID() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// resolveID uses the ticket when given, else "<typeslug>-<shortid>".
func resolveID(req SpawnRequest) string {
	if req.Ticket != "" {
		return req.Ticket
	}
	slug := strings.ReplaceAll(string(req.Type), "-", "")
	return slug + "-" + shortID()
}

// wantWorktree applies the per-type policy (design §2).
func wantWorktree(req SpawnRequest) bool {
	if req.Type.DefaultWorktree() {
		return true
	}
	return req.Worktree && (req.Type == store.TypeAnalysis || req.Type == store.TypeSpike)
}

// worktreeExists checks `git worktree list --porcelain` for an absolute path.
func (l *Lifecycle) worktreeExists(ctx context.Context, repo, rel string) (bool, error) {
	out, err := l.run.Run(ctx, repo, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git worktree list: %w", err)
	}
	want := filepath.Join(repo, rel)
	for _, line := range strings.Split(out, "\n") {
		if line == "worktree "+want {
			return true, nil
		}
	}
	return false, nil
}

// ensureWorktree creates (or adopts) the worktree and returns the branch name
// recorded on the doc (empty for a detached pr-review checkout).
func (l *Lifecycle) ensureWorktree(ctx context.Context, req SpawnRequest, id, rel string) (string, error) {
	exists, err := l.worktreeExists(ctx, req.Repo, rel)
	if err != nil {
		return "", err
	}
	if exists { // adopt
		if req.Branch != "" {
			return req.Branch, nil
		}
		return id, nil
	}
	if req.Type == store.TypePRReview && req.Branch == "" {
		// Detached worktree, then let gh resolve + fetch the PR branch.
		if out, err := l.run.Run(ctx, req.Repo, "git", "worktree", "add", "--detach", rel); err != nil {
			return "", fmt.Errorf("git worktree add --detach: %w: %s", err, out)
		}
		abs := filepath.Join(req.Repo, rel)
		if out, err := l.run.Run(ctx, abs, "gh", "pr", "checkout", req.PR); err != nil {
			return "", fmt.Errorf("gh pr checkout: %w: %s", err, out)
		}
		return "", nil
	}
	if req.Type == store.TypePRReview { // checkout the given existing branch
		if out, err := l.run.Run(ctx, req.Repo, "git", "worktree", "add", rel, req.Branch); err != nil {
			return "", fmt.Errorf("git worktree add: %w: %s", err, out)
		}
		return req.Branch, nil
	}
	// development / opt-in analysis|spike → new branch (branch = req.Branch or id).
	branch := req.Branch
	if branch == "" {
		branch = id
	}
	if out, err := l.run.Run(ctx, req.Repo, "git", "worktree", "add", rel, "-b", branch); err != nil {
		return "", fmt.Errorf("git worktree add: %w: %s", err, out)
	}
	return branch, nil
}

// Spawn resolves the id, creates a worktree per the type policy, starts a
// detached tmux session, launches claude, and returns a Session in "spawning".
func (l *Lifecycle) Spawn(ctx context.Context, req SpawnRequest) (*store.Session, error) {
	req.Type = store.NormalizeType(string(req.Type))
	id := resolveID(req)

	sess := &store.Session{
		ID:          id,
		Type:        req.Type,
		Ticket:      req.Ticket,
		TmuxSession: id,
		Repo:        req.Repo,
		PR:          req.PR,
		Status:      store.StatusSpawning,
	}

	workdir := req.Repo
	if wantWorktree(req) {
		rel := worktreeRel(id)
		branch, err := l.ensureWorktree(ctx, req, id, rel)
		if err != nil {
			return nil, err
		}
		sess.Worktree = rel
		sess.Branch = branch
		workdir = filepath.Join(req.Repo, rel)
	}

	if out, err := l.run.Run(ctx, req.Repo, "tmux", "new-session", "-d", "-s", id, "-c", workdir); err != nil {
		return nil, fmt.Errorf("tmux new-session: %w: %s", err, out)
	}
	if out, err := l.run.Run(ctx, req.Repo, "tmux", "send-keys", "-t", id, claudeCmd, "Enter"); err != nil {
		return nil, fmt.Errorf("tmux send-keys claude: %w: %s", err, out)
	}
	return sess, nil
}
