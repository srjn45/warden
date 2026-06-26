package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/client"
)

func branchesStub(t *testing.T, statuses []client.BranchStatus) string {
	return stubDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"branches": statuses})
	})
}

func TestBranchesNone(t *testing.T) {
	addr := branchesStub(t, nil)
	out, err := runCLI(t, addr, "branches")
	if err != nil {
		t.Fatalf("branches: %v", err)
	}
	if !strings.Contains(out, "No tracked branches.") {
		t.Errorf("empty branches missing notice:\n%s", out)
	}
}

func TestBranchesListed(t *testing.T) {
	addr := branchesStub(t, []client.BranchStatus{
		{
			AgentID: "code-1", Name: "alpha", Branch: "feat/x",
			CI:     client.CIStatus{State: "failure", Workflow: "ci", URL: "https://example/run/1"},
			Behind: 12, Ahead: 3,
		},
		{
			AgentID: "code-2", Branch: "feat/y",
			CI:     client.CIStatus{State: "success", Workflow: "ci"},
			Merged: true,
		},
	})
	out, err := runCLI(t, addr, "branches")
	if err != nil {
		t.Fatalf("branches: %v", err)
	}
	for _, want := range []string{
		"Branch status (2)",
		"feat/x", "code-1 (alpha)", "❌ failure", "12 behind, 3 ahead",
		"feat/y", "code-2", "✅ success", "merged into main",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("branches output missing %q:\n%s", want, out)
		}
	}
}

func TestBranchesJSON(t *testing.T) {
	addr := branchesStub(t, []client.BranchStatus{
		{AgentID: "code-1", Branch: "feat/x", CI: client.CIStatus{State: "pending"}},
	})
	out, err := runCLI(t, addr, "branches", "--json")
	if err != nil {
		t.Fatalf("branches --json: %v", err)
	}
	var got []client.BranchStatus
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got) != 1 || got[0].Branch != "feat/x" || got[0].CI.State != "pending" {
		t.Errorf("unexpected JSON payload: %+v", got)
	}
}

func TestFormatBranchVsMain(t *testing.T) {
	cases := []struct {
		s    client.BranchStatus
		want string
	}{
		{client.BranchStatus{Merged: true}, "✅ merged into main"},
		{client.BranchStatus{}, "even with main"},
		{client.BranchStatus{Behind: 4, Ahead: 1}, "4 behind, 1 ahead"},
	}
	for _, c := range cases {
		if got := formatBranchVsMain(c.s); got != c.want {
			t.Errorf("formatBranchVsMain(%+v) = %q, want %q", c.s, got, c.want)
		}
	}
}
