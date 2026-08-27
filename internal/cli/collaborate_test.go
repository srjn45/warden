package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/client"
)

// groupJoinStub returns a stub daemon that handles join/leave for a group.
func groupJoinStub(t *testing.T, result client.JoinGroupResult) string {
	t.Helper()
	return stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})
}

func groupLeaveStub(t *testing.T, result client.Group) string {
	t.Helper()
	return stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})
}

func TestCollaborateGroupJoin(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "agent-1")
	result := client.JoinGroupResult{
		Role: "orchestrator",
		Group: client.Group{
			Name: "my-team",
			Members: []client.GroupMember{
				{AgentID: "agent-1", ProjectKey: "github.com/org/repo", Summary: "backend API"},
			},
		},
	}
	addr := groupJoinStub(t, result)
	out, err := runCLI(t, addr, "collaborate", "group", "my-team", "join")
	if err != nil {
		t.Fatalf("collaborate group join: %v", err)
	}
	for _, want := range []string{"joined", "my-team", "orchestrator", "agent-1", "github.com/org/repo", "backend API"} {
		if !strings.Contains(out, want) {
			t.Errorf("join output missing %q:\n%s", want, out)
		}
	}
}

func TestCollaborateGroupLeave(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "agent-1")
	result := client.Group{
		Name:    "my-team",
		Members: []client.GroupMember{},
	}
	addr := groupLeaveStub(t, result)
	out, err := runCLI(t, addr, "collaborate", "group", "my-team", "leave")
	if err != nil {
		t.Fatalf("collaborate group leave: %v", err)
	}
	if !strings.Contains(out, "left") || !strings.Contains(out, "my-team") {
		t.Errorf("leave output unexpected:\n%s", out)
	}
}

func TestCollaborateGroupUnknownAction(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "agent-1")
	addr := stubDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	_, err := runCLI(t, addr, "collaborate", "group", "my-team", "bogus")
	if err == nil {
		t.Fatal("expected error for unknown action")
	}
}

func TestCollaborateGroupRequiresTwoArgs(t *testing.T) {
	if _, err := runCLI(t, "", "collaborate", "group", "my-team"); err == nil {
		t.Fatal("collaborate group with one arg should error")
	}
}

func TestCollaborateGroupNoAgentID(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	t.Setenv("AGENTCTL_SESSION_ID", "")
	addr := stubDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	_, err := runCLI(t, addr, "collaborate", "group", "my-team", "join")
	if err == nil {
		t.Fatal("expected error when no agent id is set")
	}
}

func TestCollaborateGroupRosterEmpty(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "agent-1")
	result := client.JoinGroupResult{
		Role:  "orchestrator",
		Group: client.Group{Name: "empty-group", Members: nil},
	}
	addr := groupJoinStub(t, result)
	out, err := runCLI(t, addr, "collaborate", "group", "empty-group", "join")
	if err != nil {
		t.Fatalf("collaborate group join empty: %v", err)
	}
	if !strings.Contains(out, "empty roster") {
		t.Errorf("expected empty roster notice:\n%s", out)
	}
}
