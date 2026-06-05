package tui

import (
	"strings"
	"testing"

	"github.com/srajanpathak/agentctl/internal/digest"
)

func TestRenderDigestLoading(t *testing.T) {
	out := renderDigest(nil, true, nil, 60)
	if !strings.Contains(out, "generating") {
		t.Errorf("loading state should say generating, got %q", out)
	}
}

func TestRenderDigestError(t *testing.T) {
	out := renderDigest(nil, false, errSample("boom"), 60)
	if !strings.Contains(out, "boom") {
		t.Errorf("error state should show error, got %q", out)
	}
}

func TestRenderDigestContent(t *testing.T) {
	d := &digest.Digest{
		Summary: "Did the work.",
		Branch:  "feat/x",
		Turns:   5,
		Status:  "idle",
		Files:   []digest.FileChange{{Path: "a.go", Added: 3, Removed: 1, Edited: true}},
	}
	out := renderDigest(d, false, nil, 60)
	for _, want := range []string{"Did the work.", "a.go", "+3", "-1", "feat/x", "idle", "5"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func errSample(s string) error { return &simpleErr{s} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }
