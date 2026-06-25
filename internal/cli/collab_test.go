package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/client"
)

func TestAgentLabel(t *testing.T) {
	if got := agentLabel(client.ConflictAgent{ID: "code-1"}); got != "code-1" {
		t.Errorf("no name = %q, want code-1", got)
	}
	if got := agentLabel(client.ConflictAgent{ID: "code-1", Name: "alpha"}); got != "code-1 (alpha)" {
		t.Errorf("with name = %q, want code-1 (alpha)", got)
	}
}

func conflictsStub(t *testing.T, conflicts []client.Conflict) string {
	return stubDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"conflicts": conflicts})
	})
}

func TestCollabConflictsNone(t *testing.T) {
	addr := conflictsStub(t, nil)
	out, err := runCLI(t, addr, "collab", "conflicts")
	if err != nil {
		t.Fatalf("collab conflicts: %v", err)
	}
	if !strings.Contains(out, "No file conflicts.") {
		t.Errorf("empty conflicts missing notice:\n%s", out)
	}
}

func TestCollabConflictsListed(t *testing.T) {
	addr := conflictsStub(t, []client.Conflict{
		{File: "a.go", Agents: []client.ConflictAgent{{ID: "code-1", Name: "alpha"}, {ID: "code-2"}}},
	})
	out, err := runCLI(t, addr, "collab", "conflicts")
	if err != nil {
		t.Fatalf("collab conflicts: %v", err)
	}
	for _, want := range []string{"File conflicts (1)", "a.go", "code-1 (alpha)", "code-2"} {
		if !strings.Contains(out, want) {
			t.Errorf("conflicts output missing %q:\n%s", want, out)
		}
	}
}

func TestCollabWhoIsEditingFound(t *testing.T) {
	addr := conflictsStub(t, []client.Conflict{
		{File: "a.go", Agents: []client.ConflictAgent{{ID: "code-1"}}},
	})
	out, err := runCLI(t, addr, "collab", "who-is-editing", "a.go")
	if err != nil {
		t.Fatalf("who-is-editing: %v", err)
	}
	if !strings.Contains(out, "Agents editing a.go:") || !strings.Contains(out, "code-1") {
		t.Errorf("who-is-editing output unexpected:\n%s", out)
	}
}

func TestCollabWhoIsEditingNotFound(t *testing.T) {
	addr := conflictsStub(t, []client.Conflict{
		{File: "a.go", Agents: []client.ConflictAgent{{ID: "code-1"}}},
	})
	out, err := runCLI(t, addr, "collab", "who-is-editing", "b.go")
	if err != nil {
		t.Fatalf("who-is-editing: %v", err)
	}
	if !strings.Contains(out, "No other agent is editing b.go.") {
		t.Errorf("expected not-found notice:\n%s", out)
	}
}

func TestCollabWhoIsEditingRequiresArg(t *testing.T) {
	if _, err := runCLI(t, "", "collab", "who-is-editing"); err == nil {
		t.Fatal("who-is-editing with no file should error")
	}
}
