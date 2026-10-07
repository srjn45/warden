package lifecycle

// COMPATIBILITY SUITE — git/check lifecycle (task t1-compat-baseline).
//
// These tests pin the behaviour that other warden features (daemon routes, MCP,
// autopilot, plan bookkeeping, guard hooks) depend on, using real temporary git
// repositories with a bare repo as origin. They exist BEFORE any behaviour change
// to the git/check surface.
//
// Later tasks may only change an assertion in this file when their task prompt
// says so, and must say why in the commit message.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// --- real-repo helpers ------------------------------------------------------

func compatGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_EDITOR=true", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

func compatIsolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func compatWrite(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
}

func compatConfigure(t *testing.T, dir string) {
	t.Helper()
	compatGit(t, dir, "config", "user.email", "t@example.com")
	compatGit(t, dir, "config", "user.name", "t")
	compatGit(t, dir, "config", "commit.gpgsign", "false")
}

func compatCommitFile(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	compatWrite(t, dir, name, content)
	compatGit(t, dir, "add", "-A")
	compatGit(t, dir, "commit", "-m", msg)
}

// compatRepo is a bare origin plus a working clone seeded with one commit on main.
type compatRepo struct{ origin, dir string }

func newCompatRepo(t *testing.T) compatRepo {
	t.Helper()
	compatIsolateGit(t)
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	compatGit(t, root, "init", "--bare", "-b", "main", origin)
	dir := filepath.Join(root, "work")
	compatGit(t, root, "init", "-b", "main", dir)
	compatConfigure(t, dir)
	compatGit(t, dir, "remote", "add", "origin", origin)
	compatCommitFile(t, dir, "f.txt", "seed\n", "seed")
	compatGit(t, dir, "push", "-u", "origin", "main")
	return compatRepo{origin: origin, dir: dir}
}

// otherClone returns a second clone of origin for advancing remote branches.
func (r compatRepo) otherClone(t *testing.T) string {
	t.Helper()
	other := filepath.Join(t.TempDir(), "other")
	compatGit(t, filepath.Dir(other), "clone", r.origin, other)
	compatConfigure(t, other)
	return other
}

func compatLife() *Lifecycle { return New(ExecRunner{}, &FakeConfig{}) }

// --- 1. JSON shape ----------------------------------------------------------

type compatField struct {
	Name      string
	Type      string
	OmitEmpty bool
}

func compatFields(t *testing.T, v any) []compatField {
	t.Helper()
	rt := reflect.TypeOf(v)
	var out []compatField
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if tag == "-" {
			continue
		}
		parts := strings.Split(tag, ",")
		f := compatField{Name: parts[0], Type: rt.Field(i).Type.String()}
		for _, p := range parts[1:] {
			if p == "omitempty" {
				f.OmitEmpty = true
			}
		}
		out = append(out, f)
	}
	return out
}

func TestCompatResultJSONFieldSets(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want []compatField
	}{
		{"CommitResult", CommitResult{}, []compatField{
			{"committed", "bool", false}, {"sha", "string", true}, {"branch", "string", false},
			{"files", "[]string", true}, {"hook_failed", "bool", true}, {"hook_output", "string", true},
			// t6 adds optional amend provenance; existing fields unchanged.
			{"amended", "bool", true}, {"warning", "string", true},
		}},
		{"PushResult", PushResult{}, []compatField{
			{"branch", "string", false}, {"remote", "string", false}, {"pushed", "bool", false},
			{"forced", "bool", true}, {"output", "string", true},
		}},
		{"SyncResult", SyncResult{}, []compatField{
			// t5-sync-default-base adds an optional provenance field; existing
			// result fields remain unchanged.
			{"branch", "string", false}, {"base", "string", false}, {"base_source", "string", true},
			{"updated", "bool", false}, {"conflicts", "[]string", true}, {"output", "string", true},
		}},
		{"CheckOutcome", CheckOutcome{}, []compatField{
			{"name", "string", false}, {"cmd", "string", false}, {"passed", "bool", false},
			{"exit_code", "int", false}, {"output", "string", true},
		}},
		{"CheckResult", CheckResult{}, []compatField{
			{"passed", "bool", false}, {"checks", "[]lifecycle.CheckOutcome", false},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, compatFields(t, tc.v))
		})
	}
}

func TestCompatResultJSONGolden(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"CommitResult-full", CommitResult{Committed: true, SHA: "abc", Branch: "b", Files: []string{"a"}, HookFailed: true, HookOutput: "x", RawBytes: 9, RawSample: "s"},
			`{"committed":true,"sha":"abc","branch":"b","files":["a"],"hook_failed":true,"hook_output":"x"}`},
		{"CommitResult-zero", CommitResult{}, `{"committed":false,"branch":""}`},
		{"PushResult-full", PushResult{Branch: "b", Remote: "origin", Pushed: true, Forced: true, Output: "o", RawBytes: 3, RawSample: "s"},
			`{"branch":"b","remote":"origin","pushed":true,"forced":true,"output":"o"}`},
		{"PushResult-zero", PushResult{}, `{"branch":"","remote":"","pushed":false}`},
		{"SyncResult-full", SyncResult{Branch: "b", Base: "main", Updated: true, Conflicts: []string{"c"}, Output: "o", RawBytes: 1, RawSample: "s"},
			`{"branch":"b","base":"main","updated":true,"conflicts":["c"],"output":"o"}`},
		{"SyncResult-zero", SyncResult{}, `{"branch":"","base":"","updated":false}`},
		{"CheckOutcome-full", CheckOutcome{Name: "n", Cmd: "c", Passed: true, ExitCode: 2, Output: "o", RawBytes: 5, RawSample: "s"},
			`{"name":"n","cmd":"c","passed":true,"exit_code":2,"output":"o"}`},
		{"CheckResult-full", CheckResult{Passed: true, Checks: []CheckOutcome{{Name: "n", Cmd: "c", Passed: true}}},
			`{"passed":true,"checks":[{"name":"n","cmd":"c","passed":true,"exit_code":0}]}`},
		{"CheckResult-zero", CheckResult{}, `{"passed":false,"checks":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.v)
			require.NoError(t, err)
			require.JSONEq(t, tc.want, string(b))
			require.Equal(t, tc.want, string(b), "field order is part of the wire shape")
		})
	}
}

// --- protected branches -----------------------------------------------------

func TestCompatIsProtectedBranch(t *testing.T) {
	require.True(t, IsProtectedBranch("main"))
	require.True(t, IsProtectedBranch("master"))
	for _, b := range []string{"", "develop", "feature", "autopilot/x", "autopilot/git-check-cli-hardening", "Main"} {
		require.False(t, IsProtectedBranch(b), b)
	}
}

func TestCompatRealRepoProtectedBranchRails(t *testing.T) {
	r := newCompatRepo(t)
	l := compatLife()
	ctx := context.Background()
	compatWrite(t, r.dir, "dirty.txt", "x\n")

	_, err := l.Commit(ctx, r.dir, "m")
	require.ErrorContains(t, err, "refusing to commit on protected branch")
	_, err = l.Push(ctx, r.dir, false)
	require.ErrorContains(t, err, "refusing to push protected branch")
	_, err = l.Push(ctx, r.dir, true)
	require.ErrorContains(t, err, "refusing to push protected branch")
	_, err = l.CreatePR(ctx, r.dir, "t", "b", "")
	require.ErrorContains(t, err, "refusing to open a PR from protected branch")
	// The refused commit must not have staged anything.
	require.Equal(t, "?? dirty.txt", compatGit(t, r.dir, "status", "--porcelain"))
}

// --- commit / push / sync against real repos --------------------------------

func TestCompatCommitPushRealRepo(t *testing.T) {
	r := newCompatRepo(t)
	l := compatLife()
	ctx := context.Background()
	compatGit(t, r.dir, "checkout", "-b", "feature")
	compatWrite(t, r.dir, "a.txt", "a\n")

	res, err := l.Commit(ctx, r.dir, "add a")
	require.NoError(t, err)
	require.True(t, res.Committed)
	require.Equal(t, "feature", res.Branch)
	require.Equal(t, []string{"a.txt"}, res.Files)
	require.Equal(t, compatGit(t, r.dir, "rev-parse", "--short", "HEAD"), res.SHA)
	require.False(t, res.HookFailed)

	clean, err := l.Commit(ctx, r.dir, "again")
	require.NoError(t, err)
	require.False(t, clean.Committed, "clean tree is a no-op, not an error")
	require.Equal(t, "feature", clean.Branch)

	pr, err := l.Push(ctx, r.dir, false)
	require.NoError(t, err)
	require.Equal(t, "feature", pr.Branch)
	require.Equal(t, "origin", pr.Remote)
	require.True(t, pr.Pushed)
	require.False(t, pr.Forced)
	require.Equal(t, compatGit(t, r.dir, "rev-parse", "HEAD"), compatGit(t, r.origin, "rev-parse", "refs/heads/feature"))

	compatGit(t, r.dir, "commit", "--amend", "-m", "amended")
	fr, err := l.Push(ctx, r.dir, true)
	require.NoError(t, err)
	require.True(t, fr.Forced)
	require.Equal(t, compatGit(t, r.dir, "rev-parse", "HEAD"), compatGit(t, r.origin, "rev-parse", "refs/heads/feature"))
}

func TestCompatCommitHookRejectionIsStructured(t *testing.T) {
	r := newCompatRepo(t)
	l := compatLife()
	compatGit(t, r.dir, "checkout", "-b", "feature")
	hook := filepath.Join(r.dir, ".git", "hooks", "pre-commit")
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\necho 'lint exploded'\nexit 1\n"), 0o755))
	compatWrite(t, r.dir, "a.txt", "a\n")

	res, err := l.Commit(context.Background(), r.dir, "m")
	require.NoError(t, err, "a hook rejection is a result, not an error")
	require.False(t, res.Committed)
	require.True(t, res.HookFailed)
	require.Contains(t, res.HookOutput, "lint exploded")
	require.Equal(t, []string{"a.txt"}, res.Files)
	require.Equal(t, "feature", res.Branch)
	require.Empty(t, compatGit(t, r.dir, "diff", "--cached", "--name-only"), "rejected commit unstages")
}

func TestCompatSyncRealRepoCleanAndConflict(t *testing.T) {
	r := newCompatRepo(t)
	l := compatLife()
	ctx := context.Background()
	compatGit(t, r.dir, "checkout", "-b", "feature")
	compatCommitFile(t, r.dir, "f.txt", "local\n", "local edit")

	other := r.otherClone(t)
	compatCommitFile(t, other, "g.txt", "g\n", "remote g")
	compatGit(t, other, "push", "origin", "main")

	res, err := l.Sync(ctx, r.dir, "")
	require.NoError(t, err)
	require.True(t, res.Updated)
	require.Equal(t, "feature", res.Branch)
	require.Equal(t, "main", res.Base, "empty base defaults to main")
	require.Empty(t, res.Conflicts)

	compatCommitFile(t, other, "f.txt", "remote\n", "remote f")
	compatGit(t, other, "push", "origin", "main")
	res, err = l.Sync(ctx, r.dir, "main")
	require.NoError(t, err, "a conflict is a result, not an error")
	require.False(t, res.Updated)
	require.Equal(t, []string{"f.txt"}, res.Conflicts)
	require.DirExists(t, filepath.Join(r.dir, ".git", "rebase-merge"), "rebase is left in progress")
}

// --- 6a. sync onto an integration branch ------------------------------------

func TestCompatAutopilotSyncExplicitIntegrationBase(t *testing.T) {
	r := newCompatRepo(t)
	l := compatLife()
	other := r.otherClone(t)
	compatGit(t, other, "checkout", "-b", "autopilot/x")
	compatCommitFile(t, other, "integ.txt", "integ\n", "integration work")
	compatGit(t, other, "push", "origin", "autopilot/x")
	compatGit(t, other, "checkout", "main")
	compatCommitFile(t, other, "main-only.txt", "m\n", "main moves on")
	compatGit(t, other, "push", "origin", "main")

	compatGit(t, r.dir, "checkout", "-b", "task-branch")
	compatCommitFile(t, r.dir, "task.txt", "t\n", "task work")

	res, err := l.Sync(context.Background(), r.dir, "autopilot/x")
	require.NoError(t, err)
	require.True(t, res.Updated)
	require.Equal(t, "autopilot/x", res.Base)
	require.Equal(t, "task-branch", res.Branch)

	integTip := compatGit(t, r.dir, "rev-parse", "origin/autopilot/x")
	compatGit(t, r.dir, "merge-base", "--is-ancestor", integTip, "HEAD")
	compatGit(t, r.dir, "fetch", "origin", "main") // sync only fetched the base; refresh main to compare
	mainTip := compatGit(t, r.dir, "rev-parse", "origin/main")
	cmd := exec.Command("git", "-C", r.dir, "merge-base", "--is-ancestor", mainTip, "HEAD")
	require.Error(t, cmd.Run(), "must rebase onto the integration branch, not main")
	require.Equal(t, "task work", compatGit(t, r.dir, "log", "-1", "--format=%s"))
	require.Equal(t, integTip, compatGit(t, r.dir, "rev-parse", "HEAD~1"))
}

// --- 6b. commit concludes a resolved merge ----------------------------------

func TestCompatCommitConcludesResolvedMerge(t *testing.T) {
	r := newCompatRepo(t)
	l := compatLife()
	other := r.otherClone(t)
	compatGit(t, other, "checkout", "-b", "autopilot/x")
	compatCommitFile(t, other, "f.txt", "integration\n", "integration edit")
	compatGit(t, other, "push", "origin", "autopilot/x")

	compatGit(t, r.dir, "checkout", "-b", "task-branch")
	compatCommitFile(t, r.dir, "f.txt", "task\n", "task edit")
	compatGit(t, r.dir, "fetch", "origin", "autopilot/x")
	merge := exec.Command("git", "-C", r.dir, "merge", "origin/autopilot/x")
	require.Error(t, merge.Run(), "merge must conflict")
	require.FileExists(t, filepath.Join(r.dir, ".git", "MERGE_HEAD"))
	compatWrite(t, r.dir, "f.txt", "resolved both\n")
	compatGit(t, r.dir, "add", "f.txt")

	res, err := l.Commit(context.Background(), r.dir, "merge autopilot/x")
	require.NoError(t, err)
	require.True(t, res.Committed)
	require.Equal(t, "task-branch", res.Branch)
	parents := strings.Fields(compatGit(t, r.dir, "rev-list", "--parents", "-n", "1", "HEAD"))
	require.Len(t, parents, 3, "commit + two parents: the merge was concluded")
	require.NoFileExists(t, filepath.Join(r.dir, ".git", "MERGE_HEAD"))
	require.Empty(t, compatGit(t, r.dir, "status", "--porcelain"))
}

// --- check ------------------------------------------------------------------

func TestCompatCheckRealConfig(t *testing.T) {
	dir := t.TempDir()
	l := compatLife()
	ctx := context.Background()

	_, err := l.Check(ctx, dir, "")
	require.ErrorIs(t, err, ErrNoCheckConfig)

	compatWrite(t, dir, ".warden/check.yml", "check:\n  zed: \"true\"\n  alpha: \"echo boom; exit 3\"\n")
	res, err := l.Check(ctx, dir, "")
	require.NoError(t, err)
	require.False(t, res.Passed)
	require.Len(t, res.Checks, 2)
	require.Equal(t, "alpha", res.Checks[0].Name, "alphabetical order")
	require.Equal(t, 3, res.Checks[0].ExitCode)
	require.Contains(t, res.Checks[0].Output, "boom")
	require.Equal(t, "zed", res.Checks[1].Name)
	require.True(t, res.Checks[1].Passed)
	require.Empty(t, res.Checks[1].Output, "passing checks carry no output")

	one, err := l.Check(ctx, dir, "zed")
	require.NoError(t, err)
	require.True(t, one.Passed)
	require.Len(t, one.Checks, 1)

	_, err = l.Check(ctx, dir, "nope")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNoCheckConfig)
	require.Contains(t, err.Error(), "alpha", "unknown-name error names the configured checks")
}

// --- t6: commit paths and amend ---------------------------------------------

func TestCommitPathsStageOnlyGiven(t *testing.T) {
	r := newCompatRepo(t)
	l := compatLife()
	ctx := context.Background()
	compatGit(t, r.dir, "checkout", "-b", "feature")
	compatWrite(t, r.dir, "a.txt", "a\n")
	compatWrite(t, r.dir, "b.txt", "b\n")
	compatWrite(t, r.dir, "sub/c.txt", "c\n")
	compatWrite(t, r.dir, "pre.txt", "pre\n")
	compatGit(t, r.dir, "add", "pre.txt") // pre-staged, must not leak in

	res, err := l.CommitWith(ctx, r.dir, CommitOptions{Message: "only a and c", Paths: []string{"a.txt", filepath.Join(r.dir, "sub", "c.txt")}})
	require.NoError(t, err)
	require.True(t, res.Committed)
	require.ElementsMatch(t, []string{"a.txt", "sub/c.txt"}, res.Files)
	require.ElementsMatch(t, []string{"a.txt", "sub/c.txt"}, strings.Fields(compatGit(t, r.dir, "show", "--name-only", "--format=", "HEAD")))
	st := compatGit(t, r.dir, "status", "--porcelain")
	require.Contains(t, st, "A  pre.txt")
	require.Contains(t, st, "?? b.txt")
}

func TestCommitPathsRejectsOutsideRepo(t *testing.T) {
	r := newCompatRepo(t)
	l := compatLife()
	compatGit(t, r.dir, "checkout", "-b", "feature")
	outside := filepath.Join(t.TempDir(), "x.txt")
	require.NoError(t, os.WriteFile(outside, []byte("x"), 0o644))
	for _, p := range []string{outside, "../escape.txt"} {
		_, err := l.CommitWith(context.Background(), r.dir, CommitOptions{Message: "m", Paths: []string{p}})
		require.ErrorContains(t, err, "outside the repository", p)
	}
}

func TestCommitNoPathsStillStagesAll(t *testing.T) {
	r := newCompatRepo(t)
	l := compatLife()
	compatGit(t, r.dir, "checkout", "-b", "feature")
	compatWrite(t, r.dir, "a.txt", "a\n")
	compatWrite(t, r.dir, "b.txt", "b\n")
	res, err := l.CommitWith(context.Background(), r.dir, CommitOptions{Message: "all"})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"a.txt", "b.txt"}, res.Files)
	require.Empty(t, compatGit(t, r.dir, "status", "--porcelain"))
}

func TestCommitAmendRetainsAndReplacesMessage(t *testing.T) {
	r := newCompatRepo(t)
	l := compatLife()
	ctx := context.Background()
	compatGit(t, r.dir, "checkout", "-b", "feature")
	compatCommitFile(t, r.dir, "a.txt", "a\n", "original msg")
	compatWrite(t, r.dir, "a.txt", "a2\n")
	compatWrite(t, r.dir, "other.txt", "o\n")

	res, err := l.CommitWith(ctx, r.dir, CommitOptions{Amend: true, Paths: []string{"a.txt"}})
	require.NoError(t, err)
	require.True(t, res.Committed)
	require.True(t, res.Amended)
	require.Empty(t, res.Warning)
	require.Equal(t, "original msg", compatGit(t, r.dir, "log", "-1", "--format=%s"))
	require.Equal(t, "a2", compatGit(t, r.dir, "show", "HEAD:a.txt"))
	require.Equal(t, "1", compatGit(t, r.dir, "rev-list", "--count", "main..HEAD"), "amend must not add a commit")
	require.Contains(t, compatGit(t, r.dir, "status", "--porcelain"), "?? other.txt")

	res, err = l.CommitWith(ctx, r.dir, CommitOptions{Amend: true, Message: "new msg"})
	require.NoError(t, err)
	require.True(t, res.Amended)
	require.Equal(t, "new msg", compatGit(t, r.dir, "log", "-1", "--format=%s"))
}

func TestCommitAmendRefusesPushedWithoutForce(t *testing.T) {
	r := newCompatRepo(t)
	l := compatLife()
	ctx := context.Background()
	compatGit(t, r.dir, "checkout", "-b", "feature")
	compatCommitFile(t, r.dir, "a.txt", "a\n", "pushed")
	_, err := l.Push(ctx, r.dir, false)
	require.NoError(t, err)
	before := compatGit(t, r.dir, "rev-parse", "HEAD")

	_, err = l.CommitWith(ctx, r.dir, CommitOptions{Amend: true, Message: "x"})
	require.ErrorContains(t, err, "--force")
	require.Equal(t, before, compatGit(t, r.dir, "rev-parse", "HEAD"))

	res, err := l.CommitWith(ctx, r.dir, CommitOptions{Amend: true, Message: "x", Force: true})
	require.NoError(t, err)
	require.True(t, res.Amended)
	require.Contains(t, res.Warning, "force-with-lease")
	require.NotEqual(t, before, compatGit(t, r.dir, "rev-parse", "HEAD"))
}

func TestCommitAmendRefusesMergeCommit(t *testing.T) {
	r := newCompatRepo(t)
	l := compatLife()
	compatGit(t, r.dir, "checkout", "-b", "feature")
	compatCommitFile(t, r.dir, "a.txt", "a\n", "a")
	compatGit(t, r.dir, "checkout", "-b", "side", "main")
	compatCommitFile(t, r.dir, "s.txt", "s\n", "s")
	compatGit(t, r.dir, "checkout", "feature")
	compatGit(t, r.dir, "merge", "--no-ff", "-m", "merge", "side")
	_, err := l.CommitWith(context.Background(), r.dir, CommitOptions{Amend: true, Message: "x", Force: true})
	require.ErrorContains(t, err, "merge commit")
}

func TestCommitAmendRefusesProtectedBranch(t *testing.T) {
	r := newCompatRepo(t)
	_, err := compatLife().CommitWith(context.Background(), r.dir, CommitOptions{Amend: true, Message: "x", Force: true})
	require.ErrorContains(t, err, "protected branch")
}
