package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srjn45/warden/internal/store"
)

func TestGuardDecision(t *testing.T) {
	isolated := &store.Session{
		ID: "code-1", Repo: "/repo",
		Worktree: ".worktrees/code-1", Workdir: "/repo/.worktrees/code-1",
	}
	inRepo := &store.Session{ID: "code-2", Repo: "/repo", Workdir: "/repo"} // no worktree
	// Workdir unset but Worktree (relative) present: the guard derives the
	// boundary from Repo+Worktree, so isolation is still enforced — it does not
	// depend on spawn having also populated Workdir.
	noWorkdir := &store.Session{ID: "code-3", Repo: "/repo", Worktree: ".worktrees/code-3"}

	cases := []struct {
		name     string
		sess     *store.Session
		tool     string
		path     string
		wantDeny bool
	}{
		{"edit inside own worktree", isolated, "Edit", "/repo/.worktrees/code-1/main.go", false},
		{"write inside own worktree", isolated, "Write", "/repo/.worktrees/code-1/sub/x.go", false},
		{"edit escapes into shared repo root", isolated, "Edit", "/repo/main.go", true},
		{"write into shared repo subdir", isolated, "Write", "/repo/internal/x.go", true},
		{"edit a sibling worktree", isolated, "Edit", "/repo/.worktrees/code-9/x.go", true},
		{"notebookedit escapes", isolated, "NotebookEdit", "/repo/nb.ipynb", true},
		{"multiedit inside worktree", isolated, "MultiEdit", "/repo/.worktrees/code-1/a.go", false},
		{"edit outside the repo entirely", isolated, "Edit", "/tmp/scratch.go", false},
		{"non-mutating tool ignored", isolated, "Read", "/repo/main.go", false},
		{"relative path fails open", isolated, "Edit", "main.go", false},
		{"in-repo agent unconstrained", inRepo, "Edit", "/repo/main.go", false},
		{"nil session allows", nil, "Edit", "/repo/main.go", false},
		// Prefix-collision guard: /repo-other must NOT count as under /repo.
		{"sibling-prefixed repo not matched", isolated, "Edit", "/repo-other/main.go", false},
		// Boundary derived from Worktree, not Workdir (Workdir unset here).
		{"escape enforced without workdir", noWorkdir, "Edit", "/repo/main.go", true},
		{"own-worktree edit allowed without workdir", noWorkdir, "Edit", "/repo/.worktrees/code-3/x.go", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deny, reason := guardDecision(tc.sess, tc.tool, tc.path)
			if deny != tc.wantDeny {
				t.Fatalf("deny = %v, want %v (reason=%q)", deny, tc.wantDeny, reason)
			}
			if deny && reason == "" {
				t.Fatal("a deny must carry a redirect reason")
			}
		})
	}
}

func postGuard(t *testing.T, s *Server, body GuardRequest) GuardResponse {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/guard", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	s.handleGuard(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp GuardResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func TestHandleGuardDeniesEscape(t *testing.T) {
	fs := newFakeStore()
	fs.Insert(context.Background(), &store.Session{
		ID: "code-1", Repo: "/repo",
		Worktree: ".worktrees/code-1", Workdir: "/repo/.worktrees/code-1",
	})
	s := &Server{store: fs}
	resp := postGuard(t, s, GuardRequest{Session: "code-1", Tool: "Edit", Path: "/repo/main.go"})
	if resp.Decision != "deny" || resp.Reason == "" {
		t.Fatalf("want deny+reason, got %+v", resp)
	}
}

func TestHandleGuardUnknownSessionFailsOpen(t *testing.T) {
	s := &Server{store: newFakeStore()}
	resp := postGuard(t, s, GuardRequest{Session: "ghost", Tool: "Edit", Path: "/repo/main.go"})
	if resp.Decision != "allow" {
		t.Fatalf("unknown session must fail open (allow), got %+v", resp)
	}
}
