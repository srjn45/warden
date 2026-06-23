//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validPipeline is a minimal two-job DAG; `pipeline create` stores and
// validates it but does not spawn jobs, so this path needs neither tmux nor
// claude.
const validPipeline = `
name: itest-demo
repo: /repo
jobs:
  - id: analyze
    prompt: "look at the code"
    worktree: none
  - id: implement
    prompt: "make the change"
    depends_on: [analyze]
    worktree: fresh
`

// invalidPipeline references an unknown dependency; ParseSpec must reject it.
const invalidPipeline = `
name: itest-bad
repo: /repo
jobs:
  - id: a
    prompt: "x"
    depends_on: [ghost]
`

// writeSpec drops a spec into the test's temp dir and returns its path.
func writeSpec(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pipeline.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	return path
}

// TestPipelineLifecycle drives create → list → show → delete end-to-end against
// the real daemon's pipeline store.
func TestPipelineLifecycle(t *testing.T) {
	h := startDaemon(t)
	spec := writeSpec(t, validPipeline)

	h.mustWd("pipeline", "create", "-f", spec)

	if list := h.mustWd("pipeline", "list"); !strings.Contains(list, "itest-demo") {
		t.Fatalf("created pipeline not in list:\n%s", list)
	}

	show := h.mustWd("pipeline", "show", "itest-demo")
	for _, want := range []string{"analyze", "implement"} {
		if !strings.Contains(show, want) {
			t.Fatalf("pipeline show missing job %q:\n%s", want, show)
		}
	}

	h.mustWd("pipeline", "delete", "itest-demo")
	if list := h.mustWd("pipeline", "list"); strings.Contains(list, "itest-demo") {
		t.Fatalf("deleted pipeline still in list:\n%s", list)
	}
}

// TestPipelineCreateRejectsInvalid confirms validation runs end-to-end: a spec
// with a dangling dependency must fail create with a non-zero exit.
func TestPipelineCreateRejectsInvalid(t *testing.T) {
	h := startDaemon(t)
	spec := writeSpec(t, invalidPipeline)

	out, err := h.wd("pipeline", "create", "-f", spec)
	if err == nil {
		t.Fatalf("expected create to fail for invalid spec, got success:\n%s", out)
	}
}
