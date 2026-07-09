package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// --- session lifecycle: restore / terminate / delete / remove-worktree / adopt ---

func TestRestoreCmd(t *testing.T) {
	method := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, nil, method, nil))
	out, err := runCLI(t, addr, "restore", "A-1")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !strings.Contains(out, "restoring A-1") {
		t.Fatalf("restore output: %q", out)
	}
	if method["/api/v1/sessions/A-1/restore"] != http.MethodPost {
		t.Fatalf("expected POST restore, got %q", method["/api/v1/sessions/A-1/restore"])
	}
}

func TestTerminateCmd(t *testing.T) {
	method := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, nil, method, nil))
	out, err := runCLI(t, addr, "terminate", "A-1")
	if err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if !strings.Contains(out, "terminated A-1") {
		t.Fatalf("terminate output: %q", out)
	}
	if method["/api/v1/sessions/A-1/terminate"] != http.MethodPost {
		t.Fatalf("expected POST terminate, got %q", method["/api/v1/sessions/A-1/terminate"])
	}
}

func TestDeleteCmdHard(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, nil, nil, body))
	out, err := runCLI(t, addr, "delete", "A-1", "--hard")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !strings.Contains(out, "deleted A-1") {
		t.Fatalf("delete output: %q", out)
	}
	if !strings.Contains(body["/api/v1/sessions/A-1/delete"], `"hard":true`) {
		t.Fatalf("--hard not forwarded: %q", body["/api/v1/sessions/A-1/delete"])
	}
}

func TestRemoveWorktreeCmdYes(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, nil, nil, body))
	out, err := runCLI(t, addr, "remove-worktree", "A-1", "--yes", "--force")
	if err != nil {
		t.Fatalf("remove-worktree: %v", err)
	}
	if !strings.Contains(out, "removed worktree for A-1") {
		t.Fatalf("remove-worktree output: %q", out)
	}
	if !strings.Contains(body["/api/v1/sessions/A-1/remove-worktree"], `"force":true`) {
		t.Fatalf("--force not forwarded: %q", body["/api/v1/sessions/A-1/remove-worktree"])
	}
}

func TestRemoveWorktreeCmdAbortsWithoutYes(t *testing.T) {
	// No --yes and a non-"y" answer on stdin must abort before any daemon call.
	called := false
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{}`))
	})
	root := newRootCmd()
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader("n\n"))
	root.SetArgs([]string{"remove-worktree", "A-1", "--addr", addr, "--config", t.TempDir() + "/none.yaml"})
	if err := root.Execute(); err != nil {
		t.Fatalf("remove-worktree: %v", err)
	}
	if !strings.Contains(out.String(), "aborted") {
		t.Fatalf("expected abort: %q", out.String())
	}
	if called {
		t.Fatal("a declined confirmation must not call the daemon")
	}
}

func TestAdoptCmd(t *testing.T) {
	t.Setenv("TMUX", "") // force resume mode (not live)
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/adopt": `{"session":{"id":"adopted-1","status":"working"},"warning":"heads up"}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "adopt", "--dir", "/work/proj")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if !strings.Contains(out, "adopted as adopted-1 (resumed)") || !strings.Contains(out, "warning: heads up") {
		t.Fatalf("adopt output: %q", out)
	}
}

// --- worktree ls / prune ---

func TestWorktreeLsCmd(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/worktrees": `{"worktrees":[{"path":".worktrees/A-1","branch":"feat","owner":"A-1","lifecycle":"live","state":"clean"}]}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "worktree", "ls", "--repo", "/repo")
	if err != nil {
		t.Fatalf("worktree ls: %v", err)
	}
	for _, want := range []string{"PATH", ".worktrees/A-1", "feat", "A-1 (live)", "clean"} {
		if !strings.Contains(out, want) {
			t.Fatalf("worktree ls missing %q:\n%s", want, out)
		}
	}
}

func TestWorktreeLsCmdJSON(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/worktrees": `{"worktrees":[{"path":".worktrees/A-1","branch":"feat"}]}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "worktree", "ls", "--repo", "/repo", "--json")
	if err != nil {
		t.Fatalf("worktree ls --json: %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if len(got) != 1 || got[0]["path"] != ".worktrees/A-1" {
		t.Fatalf("worktree ls --json payload: %s", out)
	}
}

func TestPruneCmdDryRun(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/prune": `{"results":[{"path":".worktrees/A-1","branch":"feat","owner":"","lifecycle":"","action":"remove","state":"clean"}]}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "prune", "--repo", "/repo", "--dry-run")
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	for _, want := range []string{"would remove", ".worktrees/A-1", "Summary:", "removable"} {
		if !strings.Contains(out, want) {
			t.Fatalf("prune dry-run missing %q:\n%s", want, out)
		}
	}
}

// --- auto-approve / set-permission-mode ---

func TestAutoApproveCmd(t *testing.T) {
	for _, tc := range []struct {
		mode, want string
	}{
		{"on", "auto-approve enabled for A-1"},
		{"off", "auto-approve disabled for A-1"},
	} {
		body := map[string]string{}
		addr := stubDaemon(t, routedDaemon(t, nil, nil, body))
		out, err := runCLI(t, addr, "auto-approve", "A-1", tc.mode)
		if err != nil {
			t.Fatalf("auto-approve %s: %v", tc.mode, err)
		}
		if !strings.Contains(out, tc.want) {
			t.Fatalf("auto-approve %s output: %q", tc.mode, out)
		}
		wantEnabled := tc.mode == "on"
		if wantEnabled && !strings.Contains(body["/api/v1/sessions/A-1/auto-approve"], `"enabled":true`) {
			t.Fatalf("enabled flag not forwarded: %q", body["/api/v1/sessions/A-1/auto-approve"])
		}
	}
}

func TestAutoApproveCmdInvalidMode(t *testing.T) {
	if _, err := runCLI(t, "", "auto-approve", "A-1", "maybe"); err == nil {
		t.Fatal("expected an error for an invalid mode")
	}
}

func TestAutoApproveRulesShow(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/auto-approve/policy": `{"enabled":true,"allow_sticky":false,"rules":{"allow":[{"tool":"Read"}],"deny":[]}}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "auto-approve", "rules")
	if err != nil {
		t.Fatalf("auto-approve rules: %v", err)
	}
	if !strings.Contains(out, "tool: Read") || !strings.Contains(out, "enabled: true") {
		t.Fatalf("rules output missing policy: %q", out)
	}
}

func TestAutoApproveAllowRule(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/auto-approve/policy": `{"enabled":true,"rules":{"allow":[],"deny":[]}}`,
		"PUT /api/v1/auto-approve/policy": `{"enabled":true,"rules":{"allow":[{"tool":"Read"}],"deny":[]}}`,
	}, nil, body))
	out, err := runCLI(t, addr, "auto-approve", "allow", "--tool", "Read")
	if err != nil {
		t.Fatalf("auto-approve allow: %v", err)
	}
	if !strings.Contains(out, "added allow rule") || !strings.Contains(out, "1 allow") {
		t.Fatalf("allow output unexpected: %q", out)
	}
	if !strings.Contains(body["/api/v1/auto-approve/policy"], `"tool":"Read"`) {
		t.Fatalf("rule not forwarded in PUT body: %q", body["/api/v1/auto-approve/policy"])
	}
}

func TestAutoApproveAllowEmptyRuleRejected(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/auto-approve/policy": `{"enabled":true,"rules":{"allow":[],"deny":[]}}`,
	}, nil, nil))
	if _, err := runCLI(t, addr, "auto-approve", "allow"); err == nil {
		t.Fatal("expected an error refusing an empty (match-everything) rule")
	}
}

func TestAutoApprovePerAgentAllow(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/auto-approve/policy": `{"enabled":false,"rules":{"allow":[],"deny":[]}}`,
		"PUT /api/v1/auto-approve/policy": `{"enabled":false,"rules":{"allow":[],"deny":[]},"agents":{"reviewer":{"enabled":false,"rules":{"allow":[{"tool":"Grep"}],"deny":[]}}}}`,
	}, nil, body))
	out, err := runCLI(t, addr, "auto-approve", "allow", "--agent", "reviewer", "--tool", "Grep")
	if err != nil {
		t.Fatalf("per-agent allow: %v", err)
	}
	if !strings.Contains(out, "for agent reviewer") {
		t.Fatalf("per-agent output unexpected: %q", out)
	}
	if !strings.Contains(body["/api/v1/auto-approve/policy"], `"reviewer"`) || !strings.Contains(body["/api/v1/auto-approve/policy"], `"tool":"Grep"`) {
		t.Fatalf("per-agent override not forwarded: %q", body["/api/v1/auto-approve/policy"])
	}
}

func TestSetPermissionModeCmd(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, nil, nil, body))
	out, err := runCLI(t, addr, "set-permission-mode", "A-1", "acceptEdits")
	if err != nil {
		t.Fatalf("set-permission-mode: %v", err)
	}
	if !strings.Contains(out, `permission mode set to "acceptEdits" for A-1`) {
		t.Fatalf("set-permission-mode output: %q", out)
	}
	if !strings.Contains(body["/api/v1/sessions/A-1/permission-mode"], `"permission_mode":"acceptEdits"`) {
		t.Fatalf("mode not forwarded: %q", body["/api/v1/sessions/A-1/permission-mode"])
	}
}

func TestSetPermissionModeCmdInvalid(t *testing.T) {
	if _, err := runCLI(t, "", "set-permission-mode", "A-1", "nonsense"); err == nil {
		t.Fatal("expected an error for an invalid permission mode")
	}
}

// --- set-role / role list ---

func TestSetRoleCmd(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"PATCH /api/v1/sessions/A-1/role": `{"role":"reviewer"}`,
	}, nil, body))
	out, err := runCLI(t, addr, "set-role", "A-1", "reviewer")
	if err != nil {
		t.Fatalf("set-role: %v", err)
	}
	if !strings.Contains(out, `role set to "reviewer" for A-1`) {
		t.Fatalf("set-role output: %q", out)
	}
	if !strings.Contains(body["/api/v1/sessions/A-1/role"], `"role":"reviewer"`) {
		t.Fatalf("role not forwarded: %q", body["/api/v1/sessions/A-1/role"])
	}
}

func TestSetRoleCmdInvalid(t *testing.T) {
	// A bad role name is caught client-side before any daemon call.
	if _, err := runCLI(t, "", "set-role", "A-1", "nonsense"); err == nil {
		t.Fatal("expected an error for an unknown role")
	}
}

func TestRoleListCmd(t *testing.T) {
	// Driven off the local registry; no daemon needed.
	out, err := runCLI(t, "", "role", "list")
	if err != nil {
		t.Fatalf("role list: %v", err)
	}
	for _, want := range []string{"general", "orchestrator", "implementer", "auto-merger", "reviewer", "autopilot"} {
		if !strings.Contains(out, want) {
			t.Fatalf("role list missing %q: %q", want, out)
		}
	}
}

func TestStartCmdRoleForwarded(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/spawn": `{"id":"A-9","status":"spawning"}`,
	}, nil, body))
	if _, err := runCLI(t, addr, "start", "do a thing", "--role", "reviewer"); err != nil {
		t.Fatalf("start --role: %v", err)
	}
	if !strings.Contains(body["/api/v1/spawn"], `"role":"reviewer"`) {
		t.Fatalf("role not forwarded on spawn: %q", body["/api/v1/spawn"])
	}
}

func TestStartCmdRoleInvalid(t *testing.T) {
	if _, err := runCLI(t, "", "start", "do a thing", "--role", "nonsense"); err == nil {
		t.Fatal("expected an error for an unknown --role")
	}
}

// --- send / tail / digest ---

func TestSendCmd(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, nil, nil, body))
	out, err := runCLI(t, addr, "send", "A-1", "hello", "world")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(out, "sent to A-1") {
		t.Fatalf("send output: %q", out)
	}
	if !strings.Contains(body["/api/v1/sessions/A-1/input"], `"text":"hello world"`) {
		t.Fatalf("send text not joined/forwarded: %q", body["/api/v1/sessions/A-1/input"])
	}
}

func TestTailCmd(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/sessions/A-1/output": `{"output":"pane line one\npane line two"}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "tail", "A-1", "--lines", "20")
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if !strings.Contains(out, "pane line one") {
		t.Fatalf("tail output: %q", out)
	}
}

func TestDigestCmd(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/sessions/A-1/digest": `{"summary":"did stuff","branch":"feat","turns":7,"status":"done","files":[{"path":"a.go","added":10,"removed":2,"edited":true}]}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "digest", "A-1")
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	for _, want := range []string{"did stuff", "a.go", "branch: feat", "turns: 7", "status: done"} {
		if !strings.Contains(out, want) {
			t.Fatalf("digest missing %q:\n%s", want, out)
		}
	}
}

// --- approvals / approve ---

func TestApprovalsCmd(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/approvals": `{"enabled":true,"approvals":[{"id":"A-1","question":"Run rm?","options":["Yes","No"],"fingerprint":"ff","recognized":true}]}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "approvals")
	if err != nil {
		t.Fatalf("approvals: %v", err)
	}
	for _, want := range []string{"A-1", "Run rm?", "1. Yes", "2. No", "warden approve A-1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("approvals missing %q:\n%s", want, out)
		}
	}
}

func TestApprovalsCmdDisabled(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/approvals": `{"enabled":false,"approvals":[]}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "approvals")
	if err != nil {
		t.Fatalf("approvals: %v", err)
	}
	if !strings.Contains(out, "approvals disabled") {
		t.Fatalf("approvals disabled output: %q", out)
	}
}

func TestApproveCmd(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/approvals": `{"enabled":true,"approvals":[{"id":"A-1","question":"Run rm?","options":["Yes","No"],"fingerprint":"ff","recognized":true}]}`,
	}, nil, body))
	out, err := runCLI(t, addr, "approve", "A-1", "1")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !strings.Contains(out, "approved A-1 → 1. Yes") {
		t.Fatalf("approve output: %q", out)
	}
	if !strings.Contains(body["/api/v1/sessions/A-1/approve"], `"fingerprint":"ff"`) {
		t.Fatalf("fingerprint not forwarded: %q", body["/api/v1/sessions/A-1/approve"])
	}
}

func TestApproveCmdBadOption(t *testing.T) {
	if _, err := runCLI(t, "", "approve", "A-1", "zero"); err == nil {
		t.Fatal("expected an error for a non-integer option")
	}
}

func TestApproveCmdOutOfRange(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/approvals": `{"enabled":true,"approvals":[{"id":"A-1","options":["Yes","No"],"fingerprint":"ff","recognized":true}]}`,
	}, nil, nil))
	if _, err := runCLI(t, addr, "approve", "A-1", "9"); err == nil {
		t.Fatal("expected an out-of-range error")
	}
}
