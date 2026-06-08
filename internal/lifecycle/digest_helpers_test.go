package lifecycle

import (
	"context"
	"testing"

	"github.com/srajanpathak/warden/internal/store"
)

func TestGitBranchAndNumstat(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Out: "feature/x\n"},
		"git diff --numstat":              {Out: "1\t2\tf.go\n"},
	}}
	l := New(fr)
	if b := l.GitBranch(context.Background(), "/repo"); b != "feature/x" {
		t.Errorf("GitBranch = %q, want feature/x (trimmed)", b)
	}
	if ns := l.GitNumstat(context.Background(), "/repo"); ns != "1\t2\tf.go\n" {
		t.Errorf("GitNumstat = %q", ns)
	}
}

func TestGitBranchErrorEmpty(t *testing.T) {
	fr := &FakeRunner{FailIf: func(argv []string) error { return context.Canceled }}
	l := New(fr)
	if b := l.GitBranch(context.Background(), "/notrepo"); b != "" {
		t.Errorf("GitBranch on error = %q, want empty", b)
	}
}

func TestTranscriptPathExportedWrapper(t *testing.T) {
	// No ProjectsDir set -> lookup disabled -> "".
	l := New(&FakeRunner{})
	if p := l.TranscriptPath(&store.Session{ClaudeSessionID: "abc"}); p != "" {
		t.Errorf("TranscriptPath with no ProjectsDir = %q, want empty", p)
	}
}

func TestRunClaudePExported(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{"claude -p hello": {Out: "world"}}}
	l := New(fr)
	out, err := l.RunClaudeP(context.Background(), "hello")
	if err != nil || out != "world" {
		t.Fatalf("RunClaudeP = (%q,%v), want (world,nil)", out, err)
	}
}
