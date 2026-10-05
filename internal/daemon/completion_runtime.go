package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/autopilot"
)

// autopilot.CompletionRuntime — the daemon seam for the completion phase and the
// single final PR (run-to-final-pr spec §E). Nothing here merges, approves or
// closes the final PR, and nothing writes to the default branch.

const completionGitTimeout = 5 * time.Minute

func (rt autopilotRuntime) completionHost(repo string) (daemonLandHost, bool) {
	h, ok := rt.LandingHost(repo).(daemonLandHost)
	return h, ok
}

func gitIn(ctx context.Context, dir string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, completionGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// withIntegrationWorktree runs fn in a throwaway detached worktree at
// origin/<integration>, removed afterwards.
func withIntegrationWorktree(ctx context.Context, repo, integration string, fn func(dir string) error) error {
	if _, err := gitIn(ctx, repo, "fetch", "origin"); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "warden-final-")
	if err != nil {
		return err
	}
	if _, err := gitIn(ctx, repo, "worktree", "add", "--detach", dir, "origin/"+integration); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	defer func() {
		_, _ = gitIn(context.Background(), repo, "worktree", "remove", "--force", dir)
		_ = os.RemoveAll(dir)
	}()
	return fn(dir)
}

// MergeDefault merges origin/<default> into integration (spec §E.2).
func (rt autopilotRuntime) MergeDefault(ctx context.Context, repo, integration, def string) (autopilot.MergeDefaultResult, error) {
	if _, err := gitIn(ctx, repo, "fetch", "origin"); err != nil {
		return "", err
	}
	out, err := gitIn(ctx, repo, "rev-list", "--count", "origin/"+integration+"..origin/"+def)
	if err != nil {
		return "", err
	}
	if n, _ := strconv.Atoi(strings.TrimSpace(out)); n == 0 {
		return autopilot.MergeUpToDate, nil
	}
	res := autopilot.MergeMerged
	err = withIntegrationWorktree(ctx, repo, integration, func(dir string) error {
		if _, merr := gitIn(ctx, dir, "merge", "--no-edit", "origin/"+def); merr != nil {
			_, _ = gitIn(ctx, dir, "merge", "--abort")
			res = autopilot.MergeConflict
			return nil
		}
		_, perr := gitIn(ctx, dir, "push", "origin", "HEAD:refs/heads/"+integration)
		return perr
	})
	return res, err
}

type ghFinalPR struct {
	Number     int    `json:"number"`
	URL        string `json:"url"`
	HeadRefOid string `json:"headRefOid"`
	State      string `json:"state"`
}

// EnsureFinalPR opens or adopts the one PR integration → default (spec §E.4).
func (rt autopilotRuntime) EnsureFinalPR(ctx context.Context, repo string, spec autopilot.FinalPRSpec) (autopilot.FinalPR, error) {
	h, ok := rt.completionHost(repo)
	if !ok {
		return autopilot.FinalPR{}, errors.New("final PR: no gh host for repo")
	}
	find := func() (ghFinalPR, bool, error) {
		out, err := h.runGH(ctx, "pr", "list", "--head", spec.Integration, "--base", spec.DefaultBranch,
			"--state", "open", "--json", "number,url,headRefOid,state", "--limit", "1")
		if err != nil {
			return ghFinalPR{}, false, err
		}
		var prs []ghFinalPR
		if err := json.Unmarshal([]byte(out), &prs); err != nil {
			return ghFinalPR{}, false, err
		}
		if len(prs) == 0 {
			return ghFinalPR{}, false, nil
		}
		return prs[0], true, nil
	}
	pr, found, err := find()
	if err != nil {
		return autopilot.FinalPR{}, err
	}
	if found {
		_, _ = h.runGH(ctx, "pr", "edit", strconv.Itoa(pr.Number), "--body", spec.Body) // deterministic, idempotent
	} else {
		if _, err := h.runGH(ctx, "pr", "create", "--base", spec.DefaultBranch, "--head", spec.Integration,
			"--title", spec.Title, "--body", spec.Body); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "no commits between") {
				return autopilot.FinalPR{}, autopilot.ErrNothingToMerge
			}
			return autopilot.FinalPR{}, err
		}
		if pr, found, err = find(); err != nil || !found {
			return autopilot.FinalPR{}, fmt.Errorf("final PR: created but not found: %v", err)
		}
	}
	return autopilot.FinalPR{Number: pr.Number, URL: pr.URL, HeadSHA: pr.HeadRefOid}, nil
}

// FinalPRStatus reports the final PR's state and its gate on the integration head.
func (rt autopilotRuntime) FinalPRStatus(ctx context.Context, repo, gate string, pr autopilot.FinalPR, integration string) (autopilot.FinalPRState, error) {
	h, ok := rt.completionHost(repo)
	if !ok {
		return autopilot.FinalPRState{}, errors.New("final PR: no gh host for repo")
	}
	out, err := h.runGH(ctx, "pr", "view", strconv.Itoa(pr.Number), "--json", "number,url,headRefOid,state")
	if err != nil {
		return autopilot.FinalPRState{}, err
	}
	var v ghFinalPR
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return autopilot.FinalPRState{}, err
	}
	st := autopilot.FinalPRState{State: strings.ToLower(v.State), HeadSHA: v.HeadRefOid}
	if st.State != "open" {
		return st, nil
	}
	var gs autopilot.GateState
	if gate == "local" {
		err = withIntegrationWorktree(ctx, repo, integration, func(dir string) error {
			var gerr error
			gs, st.Detail, gerr = h.GateLocal(ctx, dir)
			return gerr
		})
	} else {
		gs, st.Detail, err = h.GateCI(ctx, repo, integration, v.HeadRefOid)
	}
	if err != nil {
		return autopilot.FinalPRState{}, err
	}
	st.Gate = gs
	return st, nil
}
