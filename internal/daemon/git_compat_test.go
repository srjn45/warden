package daemon

// COMPATIBILITY SUITE — git/check daemon surface (task t1-compat-baseline).
//
// Pins the HTTP contract of POST /api/v1/git/{commit,push,sync} and /api/v1/check,
// the plan-bound session bookkeeping, plugin hook dispatch and the internal
// callers (land gate, create-pr) that other features depend on. Real temporary
// git repositories (bare origin) are used wherever the contract depends on git.
//
// Later tasks may only change an assertion in this file when their task prompt
// says so, and must say why in the commit message.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/plugin"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

// --- helpers ----------------------------------------------------------------

func cgit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_EDITOR=true", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

func cwrite(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
}

func ccommit(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	cwrite(t, dir, name, content)
	cgit(t, dir, "add", "-A")
	cgit(t, dir, "commit", "-m", msg)
}

func cconfigure(t *testing.T, dir string) {
	t.Helper()
	cgit(t, dir, "config", "user.email", "t@example.com")
	cgit(t, dir, "config", "user.name", "t")
	cgit(t, dir, "config", "commit.gpgsign", "false")
}

// compatRepo is a bare origin + a working clone seeded on main.
type compatRepo struct{ origin, dir string }

func newCompatRepo(t *testing.T) compatRepo {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	cgit(t, root, "init", "--bare", "-b", "main", origin)
	dir := filepath.Join(root, "work")
	cgit(t, root, "init", "-b", "main", dir)
	cconfigure(t, dir)
	cgit(t, dir, "remote", "add", "origin", origin)
	ccommit(t, dir, "f.txt", "seed\n", "seed")
	cgit(t, dir, "push", "-u", "origin", "main")
	return compatRepo{origin: origin, dir: dir}
}

func (r compatRepo) other(t *testing.T) string {
	t.Helper()
	o := filepath.Join(t.TempDir(), "other")
	cgit(t, filepath.Dir(o), "clone", r.origin, o)
	cconfigure(t, o)
	return o
}

// realServer wires the real lifecycle behind the real routes.
func realServer(t *testing.T) *httptest.Server {
	t.Helper()
	fs := newFakeStore()
	lc := lifecycle.New(lifecycle.ExecRunner{}, &lifecycle.FakeConfig{})
	srv := &Server{store: fs, life: NewLifecycleAdapter(lc, fs)}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)
	return ts
}

func cpost(t *testing.T, url string, body any) (int, map[string]any, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	require.NoError(t, err)
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(buf.Bytes(), &m)
	return resp.StatusCode, m, buf.String()
}

func keysOf(m map[string]any) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// --- 2. HTTP contract -------------------------------------------------------

func TestCompatHTTPCommitContract(t *testing.T) {
	ts := realServer(t)
	r := newCompatRepo(t)
	cgit(t, r.dir, "checkout", "-b", "feature")
	cwrite(t, r.dir, "a.txt", "a\n")

	code, body, raw := cpost(t, ts.URL+"/api/v1/git/commit", map[string]any{"dir": r.dir, "message": "add a"})
	require.Equal(t, 200, code, raw)
	require.Equal(t, []string{"branch", "committed", "files", "sha"}, keysOf(body))
	require.Equal(t, true, body["committed"])
	require.Equal(t, "feature", body["branch"])

	// Clean tree: 200, committed=false, only {branch, committed}.
	code, body, raw = cpost(t, ts.URL+"/api/v1/git/commit", map[string]any{"dir": r.dir, "message": "noop"})
	require.Equal(t, 200, code, raw)
	require.Equal(t, []string{"branch", "committed"}, keysOf(body))
	require.Equal(t, false, body["committed"])

	// Hook rejection: 200 + hook_failed/hook_output, never an error status.
	require.NoError(t, os.WriteFile(filepath.Join(r.dir, ".git", "hooks", "pre-commit"),
		[]byte("#!/bin/sh\necho 'hook says no'\nexit 1\n"), 0o755))
	cwrite(t, r.dir, "b.txt", "b\n")
	code, body, raw = cpost(t, ts.URL+"/api/v1/git/commit", map[string]any{"dir": r.dir, "message": "b"})
	require.Equal(t, 200, code, raw)
	require.Equal(t, true, body["hook_failed"])
	require.Contains(t, body["hook_output"], "hook says no")
	require.Equal(t, false, body["committed"])
	require.Equal(t, []string{"branch", "committed", "files", "hook_failed", "hook_output"}, keysOf(body))

	// Protected branch: 409.
	r2 := newCompatRepo(t)
	cwrite(t, r2.dir, "x.txt", "x\n")
	code, _, raw = cpost(t, ts.URL+"/api/v1/git/commit", map[string]any{"dir": r2.dir, "message": "x"})
	require.Equal(t, 409, code, raw)
	require.Contains(t, raw, "protected branch")
}

func TestCompatHTTPPushContract(t *testing.T) {
	ts := realServer(t)
	r := newCompatRepo(t)
	cgit(t, r.dir, "checkout", "-b", "feature")
	ccommit(t, r.dir, "a.txt", "a\n", "a")

	code, body, raw := cpost(t, ts.URL+"/api/v1/git/push", map[string]any{"dir": r.dir})
	require.Equal(t, 200, code, raw)
	require.Equal(t, true, body["pushed"])
	require.Equal(t, "feature", body["branch"])
	require.Equal(t, "origin", body["remote"])
	require.NotContains(t, body, "forced", "forced is omitted when false")
	require.Equal(t, cgit(t, r.dir, "rev-parse", "HEAD"), cgit(t, r.origin, "rev-parse", "refs/heads/feature"))

	cgit(t, r.dir, "commit", "--amend", "-m", "a2")
	code, body, raw = cpost(t, ts.URL+"/api/v1/git/push", map[string]any{"dir": r.dir, "force": true})
	require.Equal(t, 200, code, raw)
	require.Equal(t, true, body["forced"])

	cgit(t, r.dir, "checkout", "main")
	code, _, raw = cpost(t, ts.URL+"/api/v1/git/push", map[string]any{"dir": r.dir})
	require.Equal(t, 409, code, raw)
	require.Contains(t, raw, "protected branch")
}

func TestCompatHTTPSyncContract(t *testing.T) {
	ts := realServer(t)
	r := newCompatRepo(t)
	cgit(t, r.dir, "checkout", "-b", "feature")
	ccommit(t, r.dir, "f.txt", "local\n", "local")
	o := r.other(t)
	ccommit(t, o, "g.txt", "g\n", "g")
	cgit(t, o, "push", "origin", "main")

	code, body, raw := cpost(t, ts.URL+"/api/v1/git/sync", map[string]any{"dir": r.dir})
	require.Equal(t, 200, code, raw)
	require.Equal(t, true, body["updated"])
	require.Equal(t, "main", body["base"])
	require.Equal(t, "feature", body["branch"])
	require.NotContains(t, body, "conflicts")

	// Conflict: 200 with conflicts, rebase left in progress.
	ccommit(t, o, "f.txt", "remote\n", "f")
	cgit(t, o, "push", "origin", "main")
	code, body, raw = cpost(t, ts.URL+"/api/v1/git/sync", map[string]any{"dir": r.dir, "base": "main"})
	require.Equal(t, 200, code, raw)
	require.Equal(t, false, body["updated"])
	require.Equal(t, []any{"f.txt"}, body["conflicts"])
	require.DirExists(t, filepath.Join(r.dir, ".git", "rebase-merge"))

	// Dirty tree (mid-rebase conflict leaves unmerged paths): non-conflict failure is 409.
	cgit(t, r.dir, "rebase", "--abort")
	cwrite(t, r.dir, "dirty.txt", "d\n")
	code, _, raw = cpost(t, ts.URL+"/api/v1/git/sync", map[string]any{"dir": r.dir})
	require.Equal(t, 409, code, raw)
	require.Contains(t, raw, "uncommitted changes")
}

func TestCompatHTTPCheckContract(t *testing.T) {
	ts := realServer(t)
	dir := newCompatRepo(t).dir

	// No config: 422.
	code, _, raw := cpost(t, ts.URL+"/api/v1/check", map[string]any{"dir": dir})
	require.Equal(t, 422, code, raw)
	require.Contains(t, raw, ".warden/check.yml")

	cwrite(t, dir, ".warden/check.yml", "check:\n  ok: \"true\"\n  bad: \"echo nope; exit 1\"\n")
	code, body, raw := cpost(t, ts.URL+"/api/v1/check", map[string]any{"dir": dir})
	require.Equal(t, 200, code, raw)
	require.Equal(t, []string{"checks", "passed"}, keysOf(body))
	require.Equal(t, false, body["passed"], "a failing check is still HTTP 200")
	checks := body["checks"].([]any)
	require.Len(t, checks, 2)
	bad := checks[0].(map[string]any)
	require.Equal(t, "bad", bad["name"])
	require.Equal(t, []string{"cmd", "exit_code", "name", "output", "passed"}, keysOf(bad))

	code, body, raw = cpost(t, ts.URL+"/api/v1/check", map[string]any{"dir": dir, "name": "ok"})
	require.Equal(t, 200, code, raw)
	require.Equal(t, true, body["passed"])

	// Unknown check name: 422.
	code, _, raw = cpost(t, ts.URL+"/api/v1/check", map[string]any{"dir": dir, "name": "missing"})
	require.Equal(t, 422, code, raw)
	require.Contains(t, raw, "missing")
}

// --- 3. plan-bound bookkeeping ----------------------------------------------

type planFixture struct {
	plans *planstore.Store
	plan  *planstore.Plan
	fs    *fakeStore
	life  *fakeLife
	srv   *Server
	ts    *httptest.Server
	root  string
}

func newPlanFixture(t *testing.T, life *fakeLife) *planFixture {
	t.Helper()
	root := t.TempDir()
	plans, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = plans.Close() })
	projects, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = projects.Close() })
	_, err = projects.OpenProject(root, "test", root)
	require.NoError(t, err)
	plan := &planstore.Plan{
		ID: "plan-compat", ProjectID: root, Name: "compat-plan",
		FilePath: "plans/in_progress/compat-plan.yaml", Status: planstore.PlanStatusInProgress,
		ExecutionMode: planstore.PlanModeManual,
		ActiveExecution: &planstore.PlanExecution{
			ID: "pe-compat", PlanID: "plan-compat", ExecutionMode: planstore.PlanModeManual, ExecutorID: "agent-p",
		},
	}
	require.NoError(t, plans.Create(context.Background(), plan))
	fs := newFakeStore()
	fs.data["agent-p"] = &agentstore.Agent{ID: "agent-p", PlanID: plan.ID, Workdir: root, Status: store.StatusWorking}
	srv := &Server{store: fs, life: life, plans: plans, projects: projects}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)
	return &planFixture{plans: plans, plan: plan, fs: fs, life: life, srv: srv, ts: ts, root: root}
}

func (f *planFixture) events(t *testing.T) []*planstore.PlanExecutionEvent {
	t.Helper()
	evs, err := f.plans.ListEvents(context.Background(), f.plan.ID, "pe-compat")
	require.NoError(t, err)
	return evs
}

func (f *planFixture) kind(t *testing.T, k planstore.EventKind) []*planstore.PlanExecutionEvent {
	t.Helper()
	var out []*planstore.PlanExecutionEvent
	for _, e := range f.events(t) {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

func (f *planFixture) agentEvents(t *testing.T) map[string]string {
	t.Helper()
	a, err := f.fs.Get(context.Background(), "agent-p")
	require.NoError(t, err)
	m := map[string]string{}
	for _, e := range a.Events {
		m[e.Type] = e.Detail
	}
	return m
}

func TestCompatPlanBoundCommitBookkeeping(t *testing.T) {
	f := newPlanFixture(t, &fakeLife{gitCommitResult: lifecycle.CommitResult{Committed: true, SHA: "abc1234", Branch: "feat"}})
	code, _, raw := cpost(t, f.ts.URL+"/api/v1/git/commit", map[string]any{"session": "agent-p", "message": "m"})
	require.Equal(t, 200, code, raw)

	require.Equal(t, "abc1234 on feat", f.agentEvents(t)["commit"])
	evs := f.kind(t, planstore.EventKindCommitCreated)
	require.Len(t, evs, 1)
	require.Equal(t, "abc1234", evs[0].Payload.CommitSHA)
	require.Equal(t, "feat", evs[0].Payload.Branch)
	require.Equal(t, "agent-p", evs[0].Payload.AgentID)
	require.Equal(t, "plan-compat:pe-compat:commit_created:abc1234", evs[0].DedupKey)

	// A clean tree (Committed=false) records nothing.
	f2 := newPlanFixture(t, &fakeLife{gitCommitResult: lifecycle.CommitResult{Committed: false, Branch: "feat"}})
	code, _, raw = cpost(t, f2.ts.URL+"/api/v1/git/commit", map[string]any{"session": "agent-p"})
	require.Equal(t, 200, code, raw)
	require.Empty(t, f2.kind(t, planstore.EventKindCommitCreated))
	require.NotContains(t, f2.agentEvents(t), "commit")
}

func TestCompatPlanBoundPushRecordsAndTracksBranch(t *testing.T) {
	f := newPlanFixture(t, &fakeLife{gitPushResult: lifecycle.PushResult{Branch: "feat", Remote: "origin", Pushed: true}})
	code, _, raw := cpost(t, f.ts.URL+"/api/v1/git/push", map[string]any{"session": "agent-p"})
	require.Equal(t, 200, code, raw)
	// Idempotent: a second push must not duplicate the tracked branch.
	code, _, raw = cpost(t, f.ts.URL+"/api/v1/git/push", map[string]any{"session": "agent-p"})
	require.Equal(t, 200, code, raw)

	require.Equal(t, "feat -> origin", f.agentEvents(t)["push"])
	evs := f.kind(t, planstore.EventKindBranchPushed)
	require.Len(t, evs, 1, "dedup on plan:exec:kind:branch")
	require.Equal(t, "feat", evs[0].Payload.Branch)
	p, err := f.plans.Get(context.Background(), "plan-compat")
	require.NoError(t, err)
	require.Equal(t, []string{"feat"}, p.Branches, "trackPlanBranch records the pushed branch")
	require.Equal(t, []string{"feat"}, p.ActiveExecution.PlanBranches)
}

func TestCompatPlanBoundCheckRecordsAllWhenNoName(t *testing.T) {
	f := newPlanFixture(t, &fakeLife{checkResult: lifecycle.CheckResult{Passed: true}})
	code, _, raw := cpost(t, f.ts.URL+"/api/v1/check", map[string]any{"session": "agent-p"})
	require.Equal(t, 200, code, raw)
	require.Equal(t, "", f.life.checkName)
	evs := f.kind(t, planstore.EventKindCheckCompleted)
	require.Len(t, evs, 1)
	require.Equal(t, "all", evs[0].Payload.CheckName)
	require.Equal(t, "plan-compat:pe-compat:check_completed:all:passed", evs[0].DedupKey)
	require.Equal(t, "passed", f.agentEvents(t)["check"])

	f.life.checkResult = lifecycle.CheckResult{Passed: false}
	code, _, raw = cpost(t, f.ts.URL+"/api/v1/check", map[string]any{"session": "agent-p", "name": "lint"})
	require.Equal(t, 200, code, raw)
	require.Equal(t, "lint", f.life.checkName)
	require.Equal(t, "failed", f.agentEvents(t)["check"])
	var names []string
	for _, e := range f.kind(t, planstore.EventKindCheckCompleted) {
		names = append(names, e.Payload.CheckName)
	}
	require.ElementsMatch(t, []string{"all", "lint"}, names)
}

func TestCompatCreatePRPushesThenRecordsPROpened(t *testing.T) {
	f := newPlanFixture(t, &fakeLife{
		gitPushResult: lifecycle.PushResult{Branch: "feat", Remote: "origin", Pushed: true},
		prResult:      lifecycle.PRResult{URL: "https://example.com/pr/9", Branch: "feat", Base: "main", Created: true},
	})
	code, _, raw := cpost(t, f.ts.URL+"/api/v1/sessions/agent-p/create-pr", map[string]any{"base": "main"})
	require.Equal(t, 200, code, raw)
	require.Equal(t, f.root, f.life.gitPushDir, "create-pr pushes the session workdir first")
	require.False(t, f.life.gitPushForce, "create-pr pushes without force")
	require.Equal(t, f.root, f.life.prDir)
	require.Equal(t, "main", f.life.prBase)
	evs := f.kind(t, planstore.EventKindPROpened)
	require.Len(t, evs, 1)
	require.Equal(t, "https://example.com/pr/9", evs[0].Payload.PRURL)
	require.Equal(t, "feat", evs[0].Payload.Branch)
	require.Equal(t, "https://example.com/pr/9", f.agentEvents(t)["pr"])

	// A failed push aborts before any PR is opened: 409 "push failed: ...".
	f2 := newPlanFixture(t, &fakeLife{gitPushErr: context.DeadlineExceeded})
	code, _, raw = cpost(t, f2.ts.URL+"/api/v1/sessions/agent-p/create-pr", map[string]any{})
	require.Equal(t, 409, code, raw)
	require.Contains(t, raw, "push failed:")
	require.Empty(t, f2.life.prDir, "no PR attempted after a failed push")
	require.Empty(t, f2.kind(t, planstore.EventKindPROpened))
}

// --- 4. plugin dispatch -----------------------------------------------------

func TestCompatPluginHookEventsAndKeys(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "events.log")
	script := filepath.Join(t.TempDir(), "plug.sh")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\ncat >> \""+logPath+"\"\necho >> \""+logPath+"\"\n"), 0o755))
	reg, err := plugin.Load([]plugin.Spec{{Name: "obs", Path: script,
		Events: []string{"pre-commit", "post-commit", "pre-check", "post-check"}}})
	require.NoError(t, err)

	fs := newFakeStore()
	fs.data["agent-h"] = &agentstore.Agent{ID: "agent-h", Workdir: t.TempDir(), Status: store.StatusWorking}
	life := &fakeLife{
		gitCommitResult: lifecycle.CommitResult{Committed: true, SHA: "s1", Branch: "br"},
		checkResult:     lifecycle.CheckResult{Passed: true},
	}
	srv := &Server{store: fs, life: life}
	srv.SetPlugins(plugin.NewDispatcher(reg))
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	code, _, raw := cpost(t, ts.URL+"/api/v1/git/commit", map[string]any{"session": "agent-h", "message": "hello"})
	require.Equal(t, 200, code, raw)
	code, _, raw = cpost(t, ts.URL+"/api/v1/check", map[string]any{"session": "agent-h", "name": "lint"})
	require.Equal(t, 200, code, raw)

	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	var got []plugin.Request
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r plugin.Request
		require.NoError(t, json.Unmarshal([]byte(line), &r), line)
		got = append(got, r)
	}
	require.Len(t, got, 4)
	order := []plugin.HookEvent{plugin.EventPreCommit, plugin.EventPostCommit, plugin.EventPreCheck, plugin.EventPostCheck}
	wantPayload := []map[string]string{
		{"message": "hello"},
		{"sha": "s1", "branch": "br", "committed": "true"},
		{"name": "lint"},
		{"name": "lint", "passed": "true"},
	}
	for i := range got {
		require.Equal(t, order[i], got[i].Event)
		require.Equal(t, plugin.ProtocolVersion, got[i].ProtocolVersion)
		require.Equal(t, wantPayload[i], got[i].Payload, string(got[i].Event))
		require.Equal(t, "agent-h", got[i].Session.ID)
		require.Equal(t, fs.data["agent-h"].Workdir, got[i].Session.Workdir)
	}
}

// --- 5. internal callers ----------------------------------------------------

func TestCompatLandGateLocalRunsAllChecks(t *testing.T) {
	cases := []struct {
		name  string
		life  *fakeLife
		state autopilot.GateState
	}{
		{"pass", &fakeLife{checkResult: lifecycle.CheckResult{Passed: true}}, autopilot.GateGreen},
		{"fail", &fakeLife{checkResult: lifecycle.CheckResult{Passed: false, Checks: []lifecycle.CheckOutcome{{Name: "test", Cmd: "go test", Output: "boom"}}}}, autopilot.GateRed},
		{"no-config", &fakeLife{checkErr: lifecycle.ErrNoCheckConfig}, autopilot.GateGreen},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{store: newFakeStore(), life: tc.life}
			h := daemonLandHost{s: srv, dir: "/repo"}
			state, _, err := h.GateLocal(context.Background(), "/repo/.worktrees/w")
			require.NoError(t, err)
			require.Equal(t, tc.state, state)
			require.Equal(t, "", tc.life.checkName, "land runs life.Check(ctx, dir, \"\") — every check")
			require.Equal(t, "/repo/.worktrees/w", tc.life.checkDir, "worktree wins over host dir")

			_, _, _ = h.GateLocal(context.Background(), "")
			require.Equal(t, "/repo", tc.life.checkDir, "host dir is the fallback")
		})
	}
}
