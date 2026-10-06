package daemon

import (
	"context"
	"encoding/json"
	"github.com/srjn45/warden/internal/agentstore"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/schedule"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// newSchedServer builds a server with the scheduler gate ON and a real schedule
// store + executor, mirroring newPipeServer.
func newSchedServer(t *testing.T) (*httptest.Server, *Server, *fakeLife) {
	t.Helper()
	ps, _ := pipeline.NewStore(t.TempDir())
	cs, _ := ctxstore.New(t.TempDir())
	ss := newFakeStore()
	fl := &fakeLife{}
	exec := NewExecutor(ps, ss, fl, cs, func() {})
	sch, _ := schedule.NewStore(filepath.Join(t.TempDir(), "schedules.json"))
	srv := &Server{store: ss, life: fl, exec: exec, hub: newHub(), done: make(chan struct{})}
	srv.SetScheduler(true, sch, time.Minute)
	return httptest.NewServer(srv.router()), srv, fl
}

func TestScheduleCreateThenList(t *testing.T) {
	ts, _, _ := newSchedServer(t)
	defer ts.Close()

	body := `{"name":"daily","cron":"0 9 * * *","type":"development","repo":"/r","prompt":"do it"}`
	resp, err := http.Post(ts.URL+"/api/v1/schedules", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var sc schedule.Schedule
	json.NewDecoder(resp.Body).Decode(&sc)
	require.Equal(t, "daily", sc.ID)
	require.Equal(t, schedule.KindCron, sc.Kind)
	require.Equal(t, schedule.ModeAgent, sc.Mode)

	resp2, err := http.Get(ts.URL + "/api/v1/schedules")
	require.NoError(t, err)
	defer resp2.Body.Close()
	var lr struct {
		Schedules []schedule.Schedule `json:"schedules"`
	}
	json.NewDecoder(resp2.Body).Decode(&lr)
	require.Len(t, lr.Schedules, 1)
}

func TestScheduleCreateBothCronAndAt400(t *testing.T) {
	ts, _, _ := newSchedServer(t)
	defer ts.Close()
	body := `{"name":"bad","cron":"0 9 * * *","at":"2026-06-27T09:00:00Z","prompt":"x"}`
	resp, err := http.Post(ts.URL+"/api/v1/schedules", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestScheduleCreateBadPipelineSpec400(t *testing.T) {
	ts, _, _ := newSchedServer(t)
	defer ts.Close()
	// depends_on references a non-existent job → ParseSpec rejects it.
	body := `{"name":"p","cron":"0 9 * * *","spec":"name: p\nrepo: /r\njobs:\n  - id: a\n    prompt: go\n    depends_on: [ghost]\n"}`
	resp, err := http.Post(ts.URL+"/api/v1/schedules", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestScheduleCreateDuplicate409(t *testing.T) {
	ts, _, _ := newSchedServer(t)
	defer ts.Close()
	body := `{"name":"dup","cron":"0 9 * * *","prompt":"x","cwd":"` + t.TempDir() + `"}`
	http.Post(ts.URL+"/api/v1/schedules", "application/json", strings.NewReader(body)) //nolint:errcheck
	resp, err := http.Post(ts.URL+"/api/v1/schedules", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestScheduleDelete(t *testing.T) {
	ts, _, _ := newSchedServer(t)
	defer ts.Close()
	http.Post(ts.URL+"/api/v1/schedules", "application/json", strings.NewReader(`{"name":"s","cron":"0 9 * * *","prompt":"x","cwd":"`+t.TempDir()+`"}`)) //nolint:errcheck

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/schedules/s", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Second delete → 404.
	req2, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/schedules/s", nil)
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusNotFound, resp2.StatusCode)
}

// With the gate off every schedule endpoint returns 403.
func TestScheduleGatedOff403(t *testing.T) {
	ps, _ := pipeline.NewStore(t.TempDir())
	cs, _ := ctxstore.New(t.TempDir())
	ss := newFakeStore()
	exec := NewExecutor(ps, ss, &fakeLife{}, cs, func() {})
	srv := &Server{store: ss, life: &fakeLife{}, exec: exec, hub: newHub(), done: make(chan struct{})}
	// scheduler left OFF (no SetScheduler call).
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/schedules")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

	resp2, err := http.Post(ts.URL+"/api/v1/schedules", "application/json", strings.NewReader(`{"name":"x","cron":"0 9 * * *","prompt":"p"}`))
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusForbidden, resp2.StatusCode)
}

// A due agent schedule fires a spawn through the same life.Spawn + store.Insert
// seam the HTTP handler uses, and goes inactive (single-shot at).
func TestScheduleTickFiresAgent(t *testing.T) {
	_, srv, fl := newSchedServer(t)

	at := time.Now().Add(-time.Minute).Format(time.RFC3339)
	sc, err := schedule.New(schedule.Params{
		Name: "fire", At: at, Type: "development", Repo: "/r", Prompt: "go",
	}, time.Now())
	require.NoError(t, err)
	require.NoError(t, srv.schedStore.Create(sc))

	srv.scheduleTick(context.Background())

	require.NotNil(t, fl.spawned, "due schedule should have spawned an agent")
	require.Equal(t, "/r", fl.spawned.Repo)

	got, _ := srv.schedStore.Get("fire")
	require.False(t, got.Enabled, "single-shot at schedule should be inactive after firing")
	require.NotNil(t, got.LastRun)
	require.Empty(t, got.LastError)
}

// A due pipeline schedule creates a uniquified pipeline and reconciles it.
func TestScheduleTickFiresPipeline(t *testing.T) {
	_, srv, _ := newSchedServer(t)

	spec := "name: nightly\nrepo: /r\njobs:\n  - id: a\n    prompt: go\n    worktree: none\n"
	at := time.Now().Add(-time.Minute).Format(time.RFC3339)
	sc, err := schedule.New(schedule.Params{Name: "np", At: at, Spec: spec}, time.Now())
	require.NoError(t, err)
	require.NoError(t, srv.schedStore.Create(sc))

	srv.scheduleTick(context.Background())

	ps, err := srv.exec.pstore.List()
	require.NoError(t, err)
	require.Len(t, ps, 1, "pipeline schedule should have created one pipeline")
	require.True(t, strings.HasPrefix(ps[0].ID, "nightly-"), "fired pipeline id should be timestamp-suffixed: %s", ps[0].ID)

	got, _ := srv.schedStore.Get("np")
	require.False(t, got.Enabled)
	require.Empty(t, got.LastError)
}

// An agent-mode fire tags the spawned session with its origin schedule and
// records the run as the schedule's durable LastRunSessionID.
func TestScheduleTickTagsSessionWithScheduleID(t *testing.T) {
	_, srv, fl := newSchedServer(t)

	at := time.Now().Add(-time.Minute).Format(time.RFC3339)
	sc, err := schedule.New(schedule.Params{Name: "fire", At: at, Type: "development", Repo: "/r", Prompt: "go"}, time.Now())
	require.NoError(t, err)
	require.NoError(t, srv.schedStore.Create(sc))

	srv.scheduleTick(context.Background())

	// The inserted session carries the schedule back-ref, everywhere sessions surface.
	got, err := srv.store.Get(context.Background(), fl.spawned.ID)
	require.NoError(t, err)
	require.Equal(t, "fire", got.ScheduleID)
	require.Equal(t, "fire", got.ScheduleName)

	// And the schedule records which run it produced.
	stored, _ := srv.schedStore.Get("fire")
	require.Equal(t, fl.spawned.ID, stored.LastRunSessionID)
}

// A pipeline-mode fire propagates the schedule back-ref onto every job session
// through the executor → JobSpawnRequest → Session path.
func TestScheduleTickTagsPipelineJobSessions(t *testing.T) {
	_, srv, _ := newSchedServer(t)

	spec := "name: nightly\nrepo: /r\njobs:\n  - id: a\n    prompt: go\n    worktree: none\n"
	at := time.Now().Add(-time.Minute).Format(time.RFC3339)
	sc, err := schedule.New(schedule.Params{Name: "np", At: at, Spec: spec}, time.Now())
	require.NoError(t, err)
	require.NoError(t, srv.schedStore.Create(sc))

	srv.scheduleTick(context.Background())

	// The injected root-span-out job spawns the first real job on a follow-up
	// async Reconcile, so poll until the job session surfaces.
	var sessions []*agentstore.Agent
	require.Eventually(t, func() bool {
		got, lerr := srv.store.List(context.Background())
		if lerr != nil || len(got) == 0 {
			return false
		}
		sessions = got
		return true
	}, 2*time.Second, 5*time.Millisecond, "pipeline job session should be spawned")
	for _, s := range sessions {
		require.Equal(t, "np", s.ScheduleID, "job session %s should inherit the schedule id", s.ID)
		require.Equal(t, "np", s.ScheduleName)
	}
}

// LastRunStatus is refreshed from the live session's status on a later tick.
func TestScheduleRefreshesLastRunStatus(t *testing.T) {
	_, srv, fl := newSchedServer(t)

	at := time.Now().Add(-time.Minute).Format(time.RFC3339)
	sc, err := schedule.New(schedule.Params{Name: "fire", At: at, Type: "development", Repo: "/r", Prompt: "go"}, time.Now())
	require.NoError(t, err)
	require.NoError(t, srv.schedStore.Create(sc))
	srv.scheduleTick(context.Background())

	// Move the run's session to a terminal status, then tick again (not due).
	// fakeStore.Get returns the stored pointer, so mutating it updates the store.
	sess, _ := srv.store.Get(context.Background(), fl.spawned.ID)
	sess.Status = store.StatusDone
	srv.scheduleTick(context.Background())

	stored, _ := srv.schedStore.Get("fire")
	require.Equal(t, string(store.StatusDone), stored.LastRunStatus)
}

// GET /schedules/{id} returns one schedule, and 404 for an unknown id.
func TestGetScheduleByID(t *testing.T) {
	ts, _, _ := newSchedServer(t)
	defer ts.Close()
	http.Post(ts.URL+"/api/v1/schedules", "application/json", strings.NewReader(`{"name":"s","cron":"0 9 * * *","prompt":"x","repo":"/r"}`)) //nolint:errcheck

	resp, err := http.Get(ts.URL + "/api/v1/schedules/s")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var sc schedule.Schedule
	json.NewDecoder(resp.Body).Decode(&sc) //nolint:errcheck
	require.Equal(t, "s", sc.ID)

	resp2, err := http.Get(ts.URL + "/api/v1/schedules/ghost")
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusNotFound, resp2.StatusCode)
}

// disable clears NextRun; enable re-arms it. Both return the updated schedule.
func TestScheduleEnableDisable(t *testing.T) {
	ts, _, _ := newSchedServer(t)
	defer ts.Close()
	http.Post(ts.URL+"/api/v1/schedules", "application/json", strings.NewReader(`{"name":"s","cron":"* * * * *","prompt":"x","repo":"/r"}`)) //nolint:errcheck

	// Disable → enabled false, NextRun nil.
	resp, err := http.Post(ts.URL+"/api/v1/schedules/s/disable", "application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var d schedule.Schedule
	json.NewDecoder(resp.Body).Decode(&d) //nolint:errcheck
	require.False(t, d.Enabled)
	require.Nil(t, d.NextRun)

	// Enable → enabled true, NextRun re-armed.
	resp2, err := http.Post(ts.URL+"/api/v1/schedules/s/enable", "application/json", nil)
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	var e schedule.Schedule
	json.NewDecoder(resp2.Body).Decode(&e) //nolint:errcheck
	require.True(t, e.Enabled)
	require.NotNil(t, e.NextRun)

	// Enable/disable on an unknown id → 404.
	resp3, err := http.Post(ts.URL+"/api/v1/schedules/ghost/enable", "application/json", nil)
	require.NoError(t, err)
	defer resp3.Body.Close()
	require.Equal(t, http.StatusNotFound, resp3.StatusCode)
}

// The scheduled-agents capability is advertised end-to-end.
func TestCapabilitiesIncludesScheduledAgents(t *testing.T) {
	ts, _, _ := newSchedServer(t)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/capabilities")
	require.NoError(t, err)
	defer resp.Body.Close()
	var out struct {
		Capabilities []string `json:"capabilities"`
	}
	json.NewDecoder(resp.Body).Decode(&out) //nolint:errcheck
	require.Contains(t, out.Capabilities, "scheduled-agents")
}

// A cron schedule re-arms (stays enabled, NextRun rolls forward) after firing.
func TestScheduleTickCronReArms(t *testing.T) {
	_, srv, _ := newSchedServer(t)
	// Cron every minute so it is immediately due against a NextRun in the past.
	sc, err := schedule.New(schedule.Params{Name: "every", Cron: "* * * * *", Type: "development", Repo: "/r", Prompt: "go"}, time.Now())
	require.NoError(t, err)
	past := time.Now().Add(-time.Minute)
	sc.NextRun = &past
	require.NoError(t, srv.schedStore.Create(sc))

	srv.scheduleTick(context.Background())

	got, _ := srv.schedStore.Get("every")
	require.True(t, got.Enabled, "cron schedule should stay enabled")
	require.NotNil(t, got.NextRun)
	require.True(t, got.NextRun.After(time.Now().Add(-time.Second)), "NextRun should be re-armed to the future")
}

// postSchedule posts a create body and returns the status code and body text.
func postSchedule(t *testing.T, ts *httptest.Server, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/api/v1/schedules", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// firePastSchedule stores a due single-shot schedule and ticks the scheduler.
func firePastSchedule(t *testing.T, srv *Server, p schedule.Params) *schedule.Schedule {
	t.Helper()
	p.At = time.Now().Add(-time.Minute).Format(time.RFC3339)
	sc, err := schedule.New(p, time.Now())
	require.NoError(t, err)
	require.NoError(t, srv.schedStore.Create(sc))
	srv.scheduleTick(context.Background())
	got, err := srv.schedStore.Get(sc.ID)
	require.NoError(t, err)
	return got
}

// Regression: a prompt-only schedule (launch dir, no repo/type) was accepted
// but never fired — last_error "provide a launch dir…".
func TestScheduleTickFiresPromptOnly(t *testing.T) {
	_, srv, fl := newSchedServer(t)
	dir := t.TempDir()
	got := firePastSchedule(t, srv, schedule.Params{Name: "po", Cwd: dir, Prompt: "hi"})
	require.Empty(t, got.LastError)
	require.NotNil(t, fl.spawned, "prompt-only schedule must spawn an agent")
	require.Equal(t, dir, fl.spawnedCwd)
	require.Equal(t, "general", fl.spawned.Role)
}

// Regression: the documented default (--repo + --prompt, no type) never fired.
func TestScheduleTickFiresRepoPrompt(t *testing.T) {
	_, srv, fl := newSchedServer(t)
	got := firePastSchedule(t, srv, schedule.Params{Name: "rp", Repo: "/r", Prompt: "hi"})
	require.Empty(t, got.LastError)
	require.NotNil(t, fl.spawned)
	require.Equal(t, "/r", fl.spawned.Repo)
	require.Equal(t, "worker", fl.spawned.Role)
}

func TestScheduleTickFiresExplicitRole(t *testing.T) {
	_, srv, fl := newSchedServer(t)
	got := firePastSchedule(t, srv, schedule.Params{Name: "rr", Cwd: t.TempDir(), Role: "reviewer", Prompt: "hi"})
	require.Empty(t, got.LastError)
	require.Equal(t, "reviewer", fl.spawned.Role)
}

// A stored schedule predating cwd/role still loads; with nothing to launch in it
// records an actionable error instead of spawning.
func TestScheduleTickLegacyPromptOnlyExplainsFix(t *testing.T) {
	_, srv, fl := newSchedServer(t)
	sc := &schedule.Schedule{ID: "old", Name: "old", Kind: schedule.KindAt, Mode: schedule.ModeAgent,
		At: time.Now().Add(-time.Minute).Format(time.RFC3339), Enabled: true, Prompt: "hi", CreatedAt: time.Now()}
	require.NoError(t, schedule.Recompute(sc, time.Now().Add(-2*time.Minute)))
	require.NoError(t, srv.schedStore.Create(sc))
	srv.scheduleTick(context.Background())
	got, _ := srv.schedStore.Get("old")
	require.Nil(t, fl.spawned)
	require.Contains(t, got.LastError, "--cwd")
}

func TestScheduleFireDeletedDirExplainsFix(t *testing.T) {
	_, srv, fl := newSchedServer(t)
	dir := filepath.Join(t.TempDir(), "gone")
	sc := &schedule.Schedule{ID: "gd", Name: "gd", Kind: schedule.KindAt, Mode: schedule.ModeAgent,
		At: time.Now().Add(-time.Minute).Format(time.RFC3339), Enabled: true, Prompt: "hi", Cwd: dir, CreatedAt: time.Now()}
	require.NoError(t, schedule.Recompute(sc, time.Now().Add(-2*time.Minute)))
	require.NoError(t, srv.schedStore.Create(sc))
	srv.scheduleTick(context.Background())
	got, _ := srv.schedStore.Get("gd")
	require.Nil(t, fl.spawned)
	require.Contains(t, got.LastError, "no longer exists")
}

func TestScheduleFireNameInUseExplainsFix(t *testing.T) {
	_, srv, fl := newSchedServer(t)
	require.NoError(t, srv.store.Insert(context.Background(), &agentstore.Agent{ID: "x1", Name: "taken"}))
	got := firePastSchedule(t, srv, schedule.Params{Name: "nu", Cwd: t.TempDir(), Agent: "taken", Prompt: "hi"})
	require.Nil(t, fl.spawned)
	require.Contains(t, got.LastError, "name already in use")
	require.Contains(t, got.LastError, "--agent")
}

// Create-time validation rejects schedules that could never fire.
func TestScheduleCreateRejectsUnfireable(t *testing.T) {
	ts, _, _ := newSchedServer(t)
	defer ts.Close()
	dir := t.TempDir()
	cases := map[string]string{
		"no dir or repo": `{"name":"a","cron":"0 9 * * *","prompt":"x"}`,
		"missing dir":    `{"name":"b","cron":"0 9 * * *","prompt":"x","cwd":"` + dir + `/nope"}`,
		"unknown role":   `{"name":"c","cron":"0 9 * * *","prompt":"x","cwd":"` + dir + `","role":"wizard"}`,
		"bad type":       `{"name":"d","cron":"0 9 * * *","prompt":"x","type":"bogus","repo":"/r"}`,
	}
	for name, body := range cases {
		code, msg := postSchedule(t, ts, body)
		require.Equal(t, http.StatusBadRequest, code, "%s: %s", name, msg)
	}
}

// Create accepts every launchable form, stores cwd/role, and does not apply the
// fire-time name-in-use check.
func TestScheduleCreateAcceptsFireableForms(t *testing.T) {
	ts, srv, _ := newSchedServer(t)
	defer ts.Close()
	require.NoError(t, srv.store.Insert(context.Background(), &agentstore.Agent{ID: "x1", Name: "taken"}))
	dir := t.TempDir()
	for i, body := range []string{
		`{"name":"a","cron":"0 9 * * *","prompt":"x","cwd":"` + dir + `"}`,
		`{"name":"b","cron":"0 9 * * *","prompt":"x","repo":"/r"}`,
		`{"name":"c","cron":"0 9 * * *","prompt":"x","cwd":"` + dir + `","role":"reviewer","agent":"taken"}`,
		`{"name":"d","cron":"0 9 * * *","prompt":"x","type":"development","repo":"/r"}`,
	} {
		code, msg := postSchedule(t, ts, body)
		require.Equal(t, http.StatusCreated, code, "form %d: %s", i, msg)
	}
	got, _ := srv.schedStore.Get("c")
	require.Equal(t, dir, got.Cwd)
	require.Equal(t, "reviewer", got.Role)
}
