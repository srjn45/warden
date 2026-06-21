package approval

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		s       string
		want    bool
	}{
		{"substring fallback hit", "git push", "Bash(git push origin main)", true},
		{"substring fallback miss", "git pull", "Bash(git push origin main)", false},
		{"doublestar matches nested", "src/**", "src/a/b.go", true},
		{"doublestar no cross dir for single star", "src/*", "src/a/b.go", false},
		{"doublestar rejects other root", "src/**", "lib/x.go", false},
		{"star extension", "*.go", "main.go", true},
		{"star extension miss", "*.go", "main.rs", false},
		{"question single char", "?.go", "a.go", true},
		{"question rejects two chars", "?.go", "ab.go", false},
		{"case insensitive", "SRC/**", "src/a.go", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := globMatch(tt.pattern, tt.s); got != tt.want {
				t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pattern, tt.s, got, tt.want)
			}
		})
	}
}

func TestClassification(t *testing.T) {
	tests := []struct {
		name      string
		action    string
		wantTool  string
		wantArg   string
		wantPaths []string
	}{
		{"bash command", "Bash(rm -rf build)", "Bash", "rm -rf build", []string{"rm -rf build"}},
		{"bash with path token", "Bash(cat src/main.go)", "Bash", "cat src/main.go", []string{"cat src/main.go", "src/main.go"}},
		{"edit path", "Edit(src/a.go)", "Edit", "src/a.go", []string{"src/a.go"}},
		{"non-action", "Do you want to proceed?", "", "", nil},
		{"empty", "", "", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := Approval{Action: tt.action}
			if got := toolOf(a); got != tt.wantTool {
				t.Errorf("toolOf = %q, want %q", got, tt.wantTool)
			}
			if got := argOf(a); got != tt.wantArg {
				t.Errorf("argOf = %q, want %q", got, tt.wantArg)
			}
			got := pathsOf(a)
			if len(got) != len(tt.wantPaths) {
				t.Fatalf("pathsOf = %v, want %v", got, tt.wantPaths)
			}
			for i := range got {
				if got[i] != tt.wantPaths[i] {
					t.Errorf("pathsOf[%d] = %q, want %q", i, got[i], tt.wantPaths[i])
				}
			}
		})
	}
}

func TestClassificationEditPathIncluded(t *testing.T) {
	a := Approval{Action: "Edit(src/a.go)"}
	found := false
	for _, p := range pathsOf(a) {
		if p == "src/a.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("pathsOf(%q) = %v, want to include src/a.go", a.Action, pathsOf(a))
	}
}

func TestDecide(t *testing.T) {
	editSrc := Approval{Action: "Edit(src/a.go)", Question: "Do you want to make this edit?"}
	editSecret := Approval{Action: "Edit(secrets/.env)", Question: "Do you want to make this edit?"}
	bashPush := Approval{Action: "Bash(git push origin main)", Question: "Do you want to proceed?"}
	bashLs := Approval{Action: "Bash(ls -la)", Question: "Do you want to proceed?"}

	tests := []struct {
		name        string
		policy      Policy
		a           Approval
		wantApprove bool
		wantReason  string
	}{
		{
			name: "deny wins over allow",
			policy: Policy{Rules: Rules{
				Allow: []Rule{{Tool: "Edit"}},
				Deny:  []Rule{{Paths: []string{"src/**"}}},
			}},
			a:           editSrc,
			wantApprove: false,
			wantReason:  "matched a deny rule",
		},
		{
			name: "allow with path scope approves in-scope",
			policy: Policy{Rules: Rules{
				Allow: []Rule{{Tool: "Edit", Paths: []string{"src/**"}}},
			}},
			a:           editSrc,
			wantApprove: true,
		},
		{
			name: "allow with path scope skips out-of-scope",
			policy: Policy{Rules: Rules{
				Allow: []Rule{{Tool: "Edit", Paths: []string{"src/**"}}},
			}},
			a:           editSecret,
			wantApprove: false,
			wantReason:  "no allow rule matched",
		},
		{
			name: "pattern-only matches across tools",
			policy: Policy{Rules: Rules{
				Allow: []Rule{{Pattern: "git push"}},
			}},
			a:           bashPush,
			wantApprove: true,
		},
		{
			name: "tool-only ignores args",
			policy: Policy{Rules: Rules{
				Allow: []Rule{{Tool: "Bash"}},
			}},
			a:           bashLs,
			wantApprove: true,
		},
		{
			name:        "empty allow skips all",
			policy:      Policy{Rules: Rules{}},
			a:           bashLs,
			wantApprove: false,
			wantReason:  "no allow rule matched",
		},
		{
			name: "empty rule in allow approves anything",
			policy: Policy{Rules: Rules{
				Allow: []Rule{{}},
			}},
			a:           bashPush,
			wantApprove: true,
		},
		{
			name: "decide ignores Enabled false",
			policy: Policy{Enabled: false, Rules: Rules{
				Allow: []Rule{{Tool: "Bash"}},
			}},
			a:           bashLs,
			wantApprove: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.policy.Decide(tt.a)
			if got.Approve != tt.wantApprove {
				t.Errorf("Decide().Approve = %v, want %v (reason %q)", got.Approve, tt.wantApprove, got.Reason)
			}
			if tt.wantReason != "" && got.Reason != tt.wantReason {
				t.Errorf("Decide().Reason = %q, want %q", got.Reason, tt.wantReason)
			}
		})
	}
}

func TestPolicyUnmarshalYAML(t *testing.T) {
	t.Run("scalar true sets Enabled", func(t *testing.T) {
		var p Policy
		if err := yaml.Unmarshal([]byte("true"), &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !p.Enabled {
			t.Errorf("Enabled = false, want true")
		}
	})

	t.Run("nested mapping decodes full struct", func(t *testing.T) {
		src := `
enabled: true
allow_sticky: true
rules:
  allow:
    - tool: Edit
      paths:
        - src/**
  deny:
    - pattern: git push
`
		var p Policy
		if err := yaml.Unmarshal([]byte(src), &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !p.Enabled || !p.AllowSticky {
			t.Errorf("got enabled=%v allow_sticky=%v, want both true", p.Enabled, p.AllowSticky)
		}
		if len(p.Rules.Allow) != 1 || p.Rules.Allow[0].Tool != "Edit" ||
			len(p.Rules.Allow[0].Paths) != 1 || p.Rules.Allow[0].Paths[0] != "src/**" {
			t.Errorf("allow rules = %+v, want one Edit/src/** rule", p.Rules.Allow)
		}
		if len(p.Rules.Deny) != 1 || p.Rules.Deny[0].Pattern != "git push" {
			t.Errorf("deny rules = %+v, want one git push rule", p.Rules.Deny)
		}
	})

	t.Run("malformed scalar errors", func(t *testing.T) {
		var p Policy
		if err := yaml.Unmarshal([]byte("not-a-bool"), &p); err == nil {
			t.Errorf("expected error decoding non-bool scalar, got nil (p=%+v)", p)
		}
	})
}
