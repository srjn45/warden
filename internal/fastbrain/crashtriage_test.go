package fastbrain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const panicTrace = `panic: runtime error: invalid memory address or nil pointer dereference
[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x4a1b2c]

goroutine 42 [running]:
github.com/srjn45/warden/internal/planstore.(*Store).UpdatePlanTaskDefinition(0x0, {0xc000})
	/home/alice/dev/warden/internal/planstore/store.go:142 +0x1c
main.main()
	/home/alice/dev/warden/cmd/warden/main.go:9 +0x1d
Authorization: Bearer abcdef1234567890token
key sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV AIzaSyA1234567890abcdefghijklmnop
ghp_abcdefghijklmnopqrstuvwxyz0123 github_pat_11ABCDEFGHIJKLMNOPQRSTUV_xyz`

var secrets = []string{"sk-ant", "AIzaSy", "ghp_abc", "github_pat_", "abcdef1234567890token", "/home/alice", "alice"}

func jsonRunner(out string, got *string) Runner {
	return RunnerFunc(func(_ context.Context, p string) (string, error) {
		if got != nil {
			*got = p
		}
		return out, nil
	})
}

func TestSanitize(t *testing.T) {
	s := Sanitize(panicTrace)
	for _, bad := range secrets {
		if strings.Contains(s, bad) {
			t.Errorf("leaked %q in:\n%s", bad, s)
		}
	}
	if !strings.Contains(s, "internal/planstore/store.go:142") || strings.Contains(s, "dev/warden/internal") {
		t.Errorf("path not normalized:\n%s", s)
	}
	if got := Sanitize("cd /home/bob/proj"); got != "cd ~/proj" {
		t.Errorf("got %q", got)
	}
	if Sanitize(s) != s {
		t.Error("not idempotent")
	}
}

func TestDiagnoseModelClasses(t *testing.T) {
	for _, c := range []CrashClass{CrashInternalBug, CrashTransientError, CrashEnvironmentError, CrashTaskFailure} {
		e := NewEngine(jsonRunner(`{"class":"`+string(c)+`","confidence":0.9,"reason":"r"}`, nil), nil)
		d, err := e.DiagnoseFailure(context.Background(), CrashInput{ExitCode: 2, Excerpt: "boom"})
		if err != nil || d.Class != c || d.Source != "model" {
			t.Fatalf("%s: %+v %v", c, d, err)
		}
		if d.IsWardenBug != (c == CrashInternalBug) || d.SuggestSwitchBackend != (c == CrashTransientError) || (d.Draft != nil) != d.IsWardenBug {
			t.Errorf("%s flags wrong: %+v", c, d)
		}
	}
}

func TestDiagnoseHeuristicFailOpen(t *testing.T) {
	cases := map[CrashClass]string{
		CrashInternalBug:      panicTrace,
		CrashTransientError:   "error: 429 Too Many Requests",
		CrashEnvironmentError: `exec: "docker": executable file not found in $PATH`,
		CrashTaskFailure:      "--- FAIL: TestFoo\nFAIL",
	}
	engines := map[string]Engine{
		"nil-runner": NewEngine(nil, nil),
		"bad-json":   NewEngine(jsonRunner("not json", nil), nil),
		"bad-class":  NewEngine(jsonRunner(`{"class":"weird"}`, nil), nil),
		"error":      NewEngine(RunnerFunc(func(context.Context, string) (string, error) { return "", errors.New("x") }), nil),
	}
	for name, e := range engines {
		for want, text := range cases {
			d, err := e.DiagnoseFailure(context.Background(), CrashInput{ExitCode: 1, Excerpt: text})
			if err != nil || d.Class != want || d.Source != "heuristic" {
				t.Errorf("%s/%s: %+v %v", name, want, d, err)
			}
		}
	}
	d, err := DiagnoseFailure(context.Background(), nil, CrashInput{ExitCode: 1, Excerpt: "429"})
	if err != nil || d.Class != CrashTransientError {
		t.Errorf("nil engine: %+v %v", d, err)
	}
	// A panic with no warden frames is the user's project, not a warden bug.
	d, _ = NewEngine(nil, nil).DiagnoseFailure(context.Background(), CrashInput{ExitCode: 1, Excerpt: "panic: boom\ngoroutine 1 [running]:\nmain.f()\n\t/x/app.go:3"})
	if d.IsWardenBug {
		t.Error("foreign panic classed as warden bug")
	}
	if _, err := NewEngine(nil, nil).DiagnoseFailure(context.Background(), CrashInput{}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("empty input err = %v", err)
	}
}

func TestPromptSanitizedAndTailed(t *testing.T) {
	var prompt string
	e := NewEngine(jsonRunner(`{"class":"task_failure"}`, &prompt), nil)
	in := panicTrace + "\n" + strings.Repeat("filler\n", 100)
	if _, err := e.DiagnoseFailure(context.Background(), CrashInput{Excerpt: "head-line-secret sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV\n" + in, Command: "run --token ghp_abcdefghijklmnopqrstuvwxyz0123 /home/alice/x"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range secrets {
		if strings.Contains(prompt, bad) {
			t.Errorf("prompt leaked %q", bad)
		}
	}
	if strings.Contains(prompt, "head-line-secret") {
		t.Error("excerpt not limited to last lines")
	}
}

func TestDraftStaging(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "crashes")
	e := NewEngine(nil, nil)
	d, err := e.DiagnoseFailure(context.Background(), CrashInput{
		AgentID: "agent-1", ExitCode: 2, Excerpt: panicTrace, Version: "v9.10.1", GOOS: "linux", GOARCH: "amd64", StageDir: dir,
	})
	if err != nil || d.Draft == nil || d.DraftPath != filepath.Join(dir, "agent-1.json") {
		t.Fatalf("%+v %v", d, err)
	}
	if d.Draft.Title != "crash(planstore): nil pointer dereference in UpdatePlanTaskDefinition" {
		t.Errorf("title = %q", d.Draft.Title)
	}
	if d.Draft.Environment != "Warden v9.10.1 (linux/amd64)" {
		t.Errorf("env = %q", d.Draft.Environment)
	}
	fi, _ := os.Stat(d.DraftPath)
	di, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Errorf("perms file=%v dir=%v", fi.Mode().Perm(), di.Mode().Perm())
	}
	raw, _ := os.ReadFile(d.DraftPath)
	for _, bad := range secrets {
		if strings.Contains(string(raw), bad) {
			t.Errorf("file leaked %q", bad)
		}
	}
	got, err := LoadCrashDraft(dir, "agent-1")
	if err != nil || got.Title != d.Draft.Title || got.Status != "staged" || !strings.Contains(got.Body(), "internal/planstore/store.go:142") {
		t.Fatalf("load: %+v %v", got, err)
	}
	if l, _ := ListCrashDrafts(dir); len(l) != 1 {
		t.Errorf("list = %d", len(l))
	}
	if _, err := LoadCrashDraft(dir, "nope"); !errors.Is(err, ErrCrashNotFound) {
		t.Errorf("err = %v", err)
	}
	if _, err := LoadCrashDraft(dir, "../x"); err == nil {
		t.Error("traversal id accepted")
	}
}

func TestNonInternalDoesNotStage(t *testing.T) {
	dir := t.TempDir()
	for _, text := range []string{"429 rate limit", "make: command not found", "FAIL TestX"} {
		d, err := NewEngine(nil, nil).DiagnoseFailure(context.Background(), CrashInput{ExitCode: 1, Excerpt: text, StageDir: dir})
		if err != nil || d.Draft != nil || d.DraftPath != "" {
			t.Errorf("%q: %+v %v", text, d, err)
		}
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("files staged: %v", ents)
	}
}

func TestGeneratedIDWithoutAgent(t *testing.T) {
	d, _ := NewEngine(nil, nil).DiagnoseFailure(context.Background(), CrashInput{ExitCode: 2, Excerpt: panicTrace})
	if !strings.HasPrefix(d.Draft.ID, "crash-") || d.Draft.Environment != "Warden vunknown (unknown/unknown)" {
		t.Errorf("%+v", d.Draft)
	}
}
