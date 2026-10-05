package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/srjn45/warden/internal/store"
)

// autopilot.FixRuntime — the daemon seam for the CI fix loop
// (run-to-final-pr spec §B).

var _ autopilot.FixRuntime = autopilotRuntime{}

const (
	fixGHTimeout     = 60 * time.Second
	fixMaxFailedRuns = 3
	fixMaxJobs       = 10
	fixLogLines      = 150
	fixLogBytes      = 8 << 10
)

var fixErrLine = regexp.MustCompile(`FAIL|Error|panic|assert|error:|✗|--- FAIL`)

func fixGH(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, fixGHTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "gh", args...)
	cmd.Dir = dir
	return cmd.Output()
}

// CIEvidence lists the failing runs for the head SHA, their job names and a
// trimmed, sanitized `gh run view --log-failed` excerpt.
func (rt autopilotRuntime) CIEvidence(ctx context.Context, repo, dir, branch, headSHA string) autopilot.FixEvidence {
	if dir == "" {
		dir = repo
	}
	out, err := fixGH(ctx, dir, "run", "list", "--branch", branch,
		"--json", "databaseId,headSha,conclusion,workflowName", "--limit", "20")
	if err != nil {
		return autopilot.FixEvidence{}
	}
	var runs []struct {
		ID         int64  `json:"databaseId"`
		HeadSha    string `json:"headSha"`
		Conclusion string `json:"conclusion"`
		Workflow   string `json:"workflowName"`
	}
	if json.Unmarshal(out, &runs) != nil {
		return autopilot.FixEvidence{}
	}
	var ev autopilot.FixEvidence
	var logs []string
	for _, r := range runs {
		if r.HeadSha != headSHA {
			continue
		}
		switch r.Conclusion {
		case "failure", "timed_out", "startup_failure", "cancelled":
		default:
			continue
		}
		if len(ev.RunIDs) >= fixMaxFailedRuns {
			break
		}
		id := itoa64(r.ID)
		ev.RunIDs = append(ev.RunIDs, id)
		if len(ev.Jobs) < fixMaxJobs {
			ev.Jobs = append(ev.Jobs, r.Workflow)
		}
		if lg, err := fixGH(ctx, dir, "run", "view", id, "--log-failed"); err == nil {
			logs = append(logs, trimFailedLog(string(lg)))
		}
	}
	log := fastbrain.Sanitize(strings.Join(logs, "\n---\n"))
	if len(log) > fixLogBytes {
		log = log[len(log)-fixLogBytes:]
	}
	ev.Log = log
	return ev
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// trimFailedLog keeps the lines around failure markers (±3), else the tail,
// capped at fixLogLines.
func trimFailedLog(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	keep := map[int]bool{}
	for i, l := range lines {
		if fixErrLine.MatchString(l) {
			for j := i - 3; j <= i+3; j++ {
				if j >= 0 && j < len(lines) {
					keep[j] = true
				}
			}
		}
	}
	var sel []string
	if len(keep) == 0 {
		sel = lines
	} else {
		for i, l := range lines {
			if keep[i] {
				sel = append(sel, l)
			}
		}
	}
	if len(sel) > fixLogLines {
		sel = sel[len(sel)-fixLogLines:]
	}
	return strings.Join(sel, "\n")
}

// ClassifyCI asks Fast-Brain; it fails open to "real" (flaky=false).
func (rt autopilotRuntime) ClassifyCI(ctx context.Context, agentID, check, log string) (bool, float64, string) {
	c := fastbrain.ClassifyCIFailure(ctx, rt.s.fastBrain, fastbrain.CIInput{AgentID: agentID, Check: check, Log: log})
	return c.Class == fastbrain.CIFlakyOrInfra, c.Confidence, c.Source
}

// RerunFailed re-runs the failed jobs of each run once (authenticated gh).
func (rt autopilotRuntime) RerunFailed(ctx context.Context, repo, dir string, runIDs []string) error {
	if dir == "" {
		dir = repo
	}
	var first error
	for _, id := range runIDs {
		if _, err := fixGH(ctx, dir, "run", "rerun", id, "--failed"); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// WorkerLiveness: the session record exists, its tmux session is alive, and it
// is awaiting input (idle) or working (busy).
func (rt autopilotRuntime) WorkerLiveness(ctx context.Context, workerID string) autopilot.FixLiveness {
	sess, err := rt.s.store.Get(ctx, workerID)
	if err != nil || !liveStatus(sess.Status) {
		return autopilot.FixWorkerGone
	}
	if rt.s.poller != nil && sess.TmuxSession != "" && !rt.s.poller.SessionAlive(ctx, sess.TmuxSession) {
		return autopilot.FixWorkerGone
	}
	switch sess.Status {
	case store.StatusWaitingForInput, store.StatusIdle:
		return autopilot.FixWorkerIdle
	}
	return autopilot.FixWorkerBusy
}

// SendFix delivers the fix instruction as a real input turn.
func (rt autopilotRuntime) SendFix(ctx context.Context, workerID, msg string) error {
	return rt.WakeAgent(ctx, workerID, msg)
}

// SpawnFixWorker spawns a worker on the PR branch, taking over the dead owner's
// worktree when it survives, else adding one for the branch.
func (rt autopilotRuntime) SpawnFixWorker(ctx context.Context, spec autopilot.FixSpawn) (string, error) {
	cwd := spec.Worktree
	if cwd == "" || !dirExists(cwd) {
		var err error
		if cwd, err = ensureBranchWorktree(ctx, spec.Repo, spec.Branch); err != nil {
			return "", err
		}
	}
	req := SpawnRequest{
		Cwd: cwd, Repo: spec.Repo, Prompt: spec.Prompt, Role: "worker", Branch: spec.Branch,
		Tags:            []string{autopilotOwnTag, runTagPrefix + spec.RunID},
		AutopilotRunID:  spec.RunID,
		AutopilotTaskID: spec.TaskID,
	}
	if code, msg := rt.s.validateSpawnRequest(ctx, req); code != 0 {
		return "", errors.New(msg)
	}
	rt.s.prepareSpawnName(ctx, &req)
	sess, err := rt.s.life.Spawn(ctx, req)
	if err != nil {
		return "", err
	}
	if err := rt.s.store.Insert(ctx, sess); err != nil {
		tctx, cancel := context.WithTimeout(context.Background(), brainTeardownTimeout)
		defer cancel()
		_ = rt.s.life.Teardown(tctx, sess)
		return "", err
	}
	rt.s.notify()
	return sess.ID, nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// ensureBranchWorktree adds a worktree for an existing branch (fetching it from
// origin when it is not local) and returns its path.
func ensureBranchWorktree(ctx context.Context, repo, branch string) (string, error) {
	if repo == "" || branch == "" || strings.HasPrefix(branch, "-") {
		return "", errors.New("fix worker: repo and branch required")
	}
	path := filepath.Join(repo, ".worktrees", "fix-"+strings.NewReplacer("/", "-", " ", "-").Replace(branch))
	if dirExists(path) {
		return path, nil
	}
	run := func(args ...string) error {
		cctx, cancel := context.WithTimeout(ctx, fixGHTimeout)
		defer cancel()
		return exec.CommandContext(cctx, "git", append([]string{"-C", repo}, args...)...).Run()
	}
	if err := run("worktree", "add", path, branch); err == nil {
		return path, nil
	}
	_ = run("fetch", "origin", branch)
	if err := run("worktree", "add", "-b", branch, path, "origin/"+branch); err != nil {
		return "", err
	}
	return path, nil
}
