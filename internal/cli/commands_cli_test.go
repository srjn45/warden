package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// recordTo records the method+path+body of each request into the maps and writes
// the route's canned JSON (or `{}` for an unmatched path). It is the runCLI
// analogue of the mcp test harness: drive a real cobra command end-to-end against
// a stub daemon and assert both the wire contract and the rendered output.
func routedDaemon(t *testing.T, routes map[string]string, seenMethod, seenBody map[string]string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if seenMethod != nil {
			seenMethod[r.URL.Path] = r.Method
		}
		if seenBody != nil {
			seenBody[r.URL.Path] = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		if body, ok := routes[r.Method+" "+r.URL.Path]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}
}

// --- msg ---

func TestMsgSendCmd(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/sessions/B-2/messages": `{"message":{"id":"9","from":"human","to":"B-2","body":"hi there"},"woke":true}`,
	}, nil, body))
	out, err := runCLI(t, addr, "msg", "send", "B-2", "hi", "there")
	if err != nil {
		t.Fatalf("msg send: %v", err)
	}
	if !strings.Contains(out, "sent to B-2 (id 9)") || !strings.Contains(out, "woke recipient") {
		t.Fatalf("unexpected output: %q", out)
	}
	if !strings.Contains(body["/api/v1/sessions/B-2/messages"], `"body":"hi there"`) {
		t.Fatalf("body not joined/forwarded: %q", body["/api/v1/sessions/B-2/messages"])
	}
}

func TestMsgInboxCmd(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/sessions/A-1/messages": `{"messages":[{"id":"1","from":"B-2","body":"ping","read":false}]}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "msg", "inbox", "--as", "A-1")
	if err != nil {
		t.Fatalf("msg inbox: %v", err)
	}
	if !strings.Contains(out, "from B-2") || !strings.Contains(out, "ping") || !strings.Contains(out, "[unread]") {
		t.Fatalf("unexpected inbox output: %q", out)
	}
}

func TestMsgInboxCmdEmpty(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/sessions/A-1/messages": `{"messages":[]}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "msg", "inbox", "--as", "A-1")
	if err != nil {
		t.Fatalf("msg inbox: %v", err)
	}
	if !strings.Contains(out, "(no messages)") {
		t.Fatalf("expected empty note: %q", out)
	}
}

func TestMsgInboxCmdNeedsID(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	t.Setenv("AGENTCTL_SESSION_ID", "")
	if _, err := runCLI(t, "", "msg", "inbox"); err == nil {
		t.Fatal("expected an error with no --as and no session env")
	}
}

func TestMsgWaitCmdTimeout(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/sessions/A-1/messages/wait": `{"found":false}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "msg", "wait", "--as", "A-1", "--timeout", "1")
	if err != nil {
		t.Fatalf("msg wait: %v", err)
	}
	if !strings.Contains(out, "timed out") {
		t.Fatalf("expected timeout note: %q", out)
	}
}

// --- ctx ---

func TestCtxSetGetCmd(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	t.Setenv("AGENTCTL_SESSION_ID", "")
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"PUT /api/v1/context/global.k": `{"key":"global.k","value":"v","updated_by":"human"}`,
		"GET /api/v1/context/global.k": `{"key":"global.k","value":"the-value","updated_by":"human"}`,
	}, nil, body))

	out, err := runCLI(t, addr, "ctx", "set", "global.k", "v")
	if err != nil {
		t.Fatalf("ctx set: %v", err)
	}
	if !strings.Contains(out, "set global.k") {
		t.Fatalf("ctx set output: %q", out)
	}
	if !strings.Contains(body["/api/v1/context/global.k"], `"by":"human"`) {
		t.Fatalf("writer identity not 'human' for a non-agent shell: %q", body["/api/v1/context/global.k"])
	}

	out, err = runCLI(t, addr, "ctx", "get", "global.k")
	if err != nil {
		t.Fatalf("ctx get: %v", err)
	}
	if strings.TrimSpace(out) != "the-value" {
		t.Fatalf("ctx get output: %q", out)
	}
}

func TestCtxCASCmdConflict(t *testing.T) {
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"changed"}`))
	})
	out, err := runCLI(t, addr, "ctx", "cas", "k", "v", "--expected", "old")
	if err == nil {
		t.Fatal("expected a conflict error")
	}
	if !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("expected a friendly conflict message, got %v (out=%q)", err, out)
	}
}

func TestCtxAppendCmd(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/context/log/append": `{"key":"log","value":"a\nb","updated_by":"human"}`,
	}, nil, body))
	out, err := runCLI(t, addr, "ctx", "append", "log", "b")
	if err != nil {
		t.Fatalf("ctx append: %v", err)
	}
	if !strings.Contains(out, "appended to log") {
		t.Fatalf("ctx append output: %q", out)
	}
}

func TestCtxListCmd(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/context": `{"entries":[{"key":"global.a","value":"1","updated_by":"A-1"}]}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "ctx", "list", "global.")
	if err != nil {
		t.Fatalf("ctx list: %v", err)
	}
	if !strings.Contains(out, "global.a") || !strings.Contains(out, "A-1") {
		t.Fatalf("ctx list output: %q", out)
	}
}

func TestCtxDelCmd(t *testing.T) {
	method := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, nil, method, nil))
	out, err := runCLI(t, addr, "ctx", "del", "k")
	if err != nil {
		t.Fatalf("ctx del: %v", err)
	}
	if !strings.Contains(out, "deleted k") {
		t.Fatalf("ctx del output: %q", out)
	}
	if method["/api/v1/context/k"] != http.MethodDelete {
		t.Fatalf("expected DELETE, got %q", method["/api/v1/context/k"])
	}
}

func TestCtxDelCmdRejectsBadKey(t *testing.T) {
	// The client rejects slash keys before any daemon call; the CLI surfaces it.
	if _, err := runCLI(t, "", "ctx", "del", "bad/key"); err == nil {
		t.Fatal("expected an error for an invalid key")
	}
}

// --- pipeline ---

func TestPipelineListShowCmds(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/pipelines":      `{"pipelines":[{"id":"demo","status":"running","jobs":[{"id":"a"},{"id":"b"}]}]}`,
		"GET /api/v1/pipelines/demo": `{"id":"demo","status":"running","repo":"/r","jobs":[{"id":"a","status":"done","branch":"feat","output":"result"}]}`,
	}, nil, nil))

	out, err := runCLI(t, addr, "pipeline", "list")
	if err != nil {
		t.Fatalf("pipeline list: %v", err)
	}
	if !strings.Contains(out, "demo") || !strings.Contains(out, "2 jobs") {
		t.Fatalf("pipeline list output: %q", out)
	}

	out, err = runCLI(t, addr, "pipeline", "show", "demo")
	if err != nil {
		t.Fatalf("pipeline show: %v", err)
	}
	for _, want := range []string{"demo", "running", "branch: feat", "output: result"} {
		if !strings.Contains(out, want) {
			t.Fatalf("pipeline show missing %q:\n%s", want, out)
		}
	}
}

func TestPipelineLifecycleCmds(t *testing.T) {
	method := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, nil, method, nil))
	cases := []struct {
		args     []string
		path     string
		method   string
		wantText string
	}{
		{[]string{"pipeline", "start", "demo"}, "/api/v1/pipelines/demo/start", http.MethodPost, "started demo"},
		{[]string{"pipeline", "pause", "demo"}, "/api/v1/pipelines/demo/pause", http.MethodPost, "paused demo"},
		{[]string{"pipeline", "resume", "demo"}, "/api/v1/pipelines/demo/resume", http.MethodPost, "resumed demo"},
		{[]string{"pipeline", "cancel", "demo"}, "/api/v1/pipelines/demo/cancel", http.MethodPost, "canceled demo"},
		{[]string{"pipeline", "delete", "demo"}, "/api/v1/pipelines/demo", http.MethodDelete, "deleted demo"},
		{[]string{"pipeline", "retry", "demo", "a"}, "/api/v1/pipelines/demo/jobs/a/retry", http.MethodPost, "retrying demo/a"},
	}
	for _, tc := range cases {
		out, err := runCLI(t, addr, tc.args...)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if !strings.Contains(out, tc.wantText) {
			t.Fatalf("%v output: %q", tc.args, out)
		}
		if method[tc.path] != tc.method {
			t.Fatalf("%v: expected %s %s, got %s", tc.args, tc.method, tc.path, method[tc.path])
		}
	}
}

func TestPipelineEmitCmd(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, nil, nil, body))
	out, err := runCLI(t, addr, "pipeline", "emit", "done", "--pipeline", "demo", "--job", "a")
	if err != nil {
		t.Fatalf("pipeline emit: %v", err)
	}
	if !strings.Contains(out, "emitted handoff for demo/a") {
		t.Fatalf("emit output: %q", out)
	}
	if !strings.Contains(body["/api/v1/pipelines/demo/jobs/a/emit"], `"text":"done"`) {
		t.Fatalf("emit body: %q", body["/api/v1/pipelines/demo/jobs/a/emit"])
	}
}

func TestPipelineEmitCmdNeedsContext(t *testing.T) {
	t.Setenv("WARDEN_PIPELINE_ID", "")
	t.Setenv("AGENTCTL_PIPELINE_ID", "")
	t.Setenv("WARDEN_JOB_ID", "")
	t.Setenv("AGENTCTL_JOB_ID", "")
	if _, err := runCLI(t, "", "pipeline", "emit", "done"); err == nil {
		t.Fatal("expected an error without pipeline/job context")
	}
}

func TestPipelineEditJobCmd(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, nil, nil, body))
	out, err := runCLI(t, addr, "pipeline", "edit-job", "demo", "a", "--prompt", "new prompt")
	if err != nil {
		t.Fatalf("edit-job: %v", err)
	}
	if !strings.Contains(out, "edited demo/a") {
		t.Fatalf("edit-job output: %q", out)
	}
	if !strings.Contains(body["/api/v1/pipelines/demo/jobs/a/edit"], `"prompt":"new prompt"`) {
		t.Fatalf("edit-job body: %q", body["/api/v1/pipelines/demo/jobs/a/edit"])
	}
}

func TestPipelineEditJobCmdNeedsFlag(t *testing.T) {
	if _, err := runCLI(t, "", "pipeline", "edit-job", "demo", "a"); err == nil {
		t.Fatal("expected an error when neither --prompt nor --handoff is given")
	}
}

// --- schedule ---

func TestScheduleCreateCmd(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/schedules": `{"id":"nightly","name":"nightly","kind":"cron","mode":"agent","enabled":true}`,
	}, nil, body))
	out, err := runCLI(t, addr, "schedule", "create", "nightly", "--cron", "0 9 * * *", "--type", "development", "--repo", "/r", "--prompt", "go")
	if err != nil {
		t.Fatalf("schedule create: %v", err)
	}
	if !strings.Contains(out, "created schedule nightly") {
		t.Fatalf("schedule create output: %q", out)
	}
	if !strings.Contains(body["/api/v1/schedules"], `"cron":"0 9 * * *"`) {
		t.Fatalf("schedule create body: %q", body["/api/v1/schedules"])
	}
}

func TestScheduleListCmd(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/schedules": `{"schedules":[{"id":"nightly","name":"nightly","kind":"cron","mode":"agent","cron":"0 9 * * *","enabled":true}]}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "schedule", "list")
	if err != nil {
		t.Fatalf("schedule list: %v", err)
	}
	if !strings.Contains(out, "nightly") || !strings.Contains(out, "enabled") {
		t.Fatalf("schedule list output: %q", out)
	}
}

func TestScheduleDeleteCmd(t *testing.T) {
	method := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, nil, method, nil))
	out, err := runCLI(t, addr, "schedule", "delete", "nightly")
	if err != nil {
		t.Fatalf("schedule delete: %v", err)
	}
	if !strings.Contains(out, "deleted nightly") {
		t.Fatalf("schedule delete output: %q", out)
	}
	if method["/api/v1/schedules/nightly"] != http.MethodDelete {
		t.Fatalf("expected DELETE, got %q", method["/api/v1/schedules/nightly"])
	}
}

// --- snapshot ---

func TestSnapshotCreateCmd(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "A-1")
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/snapshots": `{"id":"snap-1","branch":"feat","head_sha":"abcdef1234","dirty_files":["a.go"],"stash_sha":"deadbeef99","transcript_path":"/t/x.log","transcript_lines":42}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "snapshot", "create", "-m", "good point")
	if err != nil {
		t.Fatalf("snapshot create: %v", err)
	}
	for _, want := range []string{"snapshot snap-1 captured on feat", "1 uncommitted file(s) stashed", "transcript: /t/x.log (42 lines)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("snapshot create missing %q:\n%s", want, out)
		}
	}
}

func TestSnapshotListCmd(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/snapshots": `{"snapshots":[{"id":"snap-1","branch":"feat","head_sha":"abcdef1234","message":"checkpoint"}]}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "snapshot", "list", "--all")
	if err != nil {
		t.Fatalf("snapshot list: %v", err)
	}
	if !strings.Contains(out, "snap-1") || !strings.Contains(out, "checkpoint") {
		t.Fatalf("snapshot list output: %q", out)
	}
}

func TestSnapshotRestoreCmd(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/snapshots/snap-1/restore": `{"snapshot_id":"snap-1","branch":"feat","applied":true,"head_match":true}`,
	}, nil, body))
	out, err := runCLI(t, addr, "snapshot", "restore", "snap-1", "--force")
	if err != nil {
		t.Fatalf("snapshot restore: %v", err)
	}
	if !strings.Contains(out, "restored snap-1 onto feat") {
		t.Fatalf("snapshot restore output: %q", out)
	}
	if !strings.Contains(body["/api/v1/snapshots/snap-1/restore"], `"force":true`) {
		t.Fatalf("force not forwarded: %q", body["/api/v1/snapshots/snap-1/restore"])
	}
}

func TestSnapshotRestoreCmdConflicts(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/snapshots/snap-1/restore": `{"snapshot_id":"snap-1","branch":"feat","applied":true,"head_match":false,"conflicts":["a.go"],"snapshot_head":"aaaa1111","current_head":"bbbb2222"}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "snapshot", "restore", "snap-1")
	if err != nil {
		t.Fatalf("snapshot restore: %v", err)
	}
	for _, want := range []string{"with conflicts", "a.go", "HEAD moved since capture"} {
		if !strings.Contains(out, want) {
			t.Fatalf("snapshot restore missing %q:\n%s", want, out)
		}
	}
}

// --- search / history ---

func TestSearchCmd(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/search": `{"sessions":[{"id":"A-1","name":"alpha","subject":"do x"}]}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "search", "alpha")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !strings.Contains(out, "A-1") || !strings.Contains(out, "alpha") {
		t.Fatalf("search output: %q", out)
	}
}

func TestSearchCmdJSON(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/search": `{"sessions":[{"id":"A-1"}]}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "search", "x", "--json", "--closed")
	if err != nil {
		t.Fatalf("search --json: %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("search --json not valid JSON: %v\n%s", err, out)
	}
	if len(got) != 1 || got[0]["id"] != "A-1" {
		t.Fatalf("search --json payload: %s", out)
	}
}

func TestHistoryCmd(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/history": `{"sessions":[{"id":"A-1","name":"alpha","subject":"old work"}]}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "history", "--since", "7d", "--type", "development")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if !strings.Contains(out, "A-1") || !strings.Contains(out, "old work") {
		t.Fatalf("history output: %q", out)
	}
}

func TestHistoryCmdEmpty(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/history": `{"sessions":[]}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "history")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if !strings.Contains(out, "no archived agents match") {
		t.Fatalf("history empty output: %q", out)
	}
}

func TestHistoryCmdBadSince(t *testing.T) {
	if _, err := runCLI(t, "", "history", "--since", "nonsense"); err == nil {
		t.Fatal("expected an error for an invalid --since")
	}
}

// --- check (commit/push/sync already covered in git_test.go) ---

func TestCheckCmdPass(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/check": `{"passed":true,"checks":[{"name":"test","cmd":"go test","passed":true}]}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "check")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !strings.Contains(out, "✓ test") {
		t.Fatalf("check pass output: %q", out)
	}
}

func TestCheckCmdFailExitsNonZero(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/check": `{"passed":false,"checks":[{"name":"test","cmd":"go test","passed":false,"exit_code":1,"output":"FAIL"}]}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "check", "test")
	if err == nil {
		t.Fatal("a failing check must return a non-nil error so scripts/CI see a non-zero exit")
	}
	if !strings.Contains(out, "✗ test") || !strings.Contains(out, "FAIL") {
		t.Fatalf("check fail output: %q", out)
	}
}
