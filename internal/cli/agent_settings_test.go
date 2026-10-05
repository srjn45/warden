package cli

import (
	"strings"
	"testing"
)

func TestAgentSetRoundTripsThroughLegacyRequests(t *testing.T) {
	cases := []struct {
		key, value string
		legacy     []string
		path       string
	}{
		{"permission-mode", "acceptEdits", []string{"set-permission-mode", "A-1", "acceptEdits"}, "/api/v1/sessions/A-1/permission-mode"},
		{"compact", "on", []string{"force-compact", "A-1", "on"}, "/api/v1/sessions/A-1/force-compact"},
		{"role", "reviewer", []string{"set-role", "A-1", "reviewer"}, "/api/v1/sessions/A-1/role"},
		{"auto-approve", "on", []string{"auto-approve", "A-1", "on"}, "/api/v1/sessions/A-1/auto-approve"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			newBody, oldBody := map[string]string{}, map[string]string{}
			newAddr := stubDaemon(t, routedDaemon(t, nil, nil, newBody))
			oldAddr := stubDaemon(t, routedDaemon(t, nil, nil, oldBody))
			newOut, err := runCLI(t, newAddr, "agent", "set", "A-1", tc.key, tc.value)
			if err != nil {
				t.Fatalf("agent set: %v", err)
			}
			oldOut, err := runCLI(t, oldAddr, tc.legacy...)
			if err != nil {
				t.Fatalf("legacy: %v", err)
			}
			if newBody[tc.path] == "" || newBody[tc.path] != oldBody[tc.path] {
				t.Fatalf("request mismatch: new=%q old=%q", newBody[tc.path], oldBody[tc.path])
			}
			if newOut != oldOut {
				t.Fatalf("output mismatch: %q vs %q", newOut, oldOut)
			}
		})
	}
}

func TestAgentSetUnknownKeyAndValueListChoices(t *testing.T) {
	_, err := runCLI(t, "", "agent", "set", "A-1", "bogus", "x")
	if err == nil || !strings.Contains(err.Error(), "permission-mode, compact, role, auto-approve") {
		t.Fatalf("unknown key err: %v", err)
	}
	_, err = runCLI(t, "", "agent", "set", "A-1", "permission-mode", "nope")
	if err == nil || !strings.Contains(err.Error(), "acceptEdits") {
		t.Fatalf("bad value err: %v", err)
	}
	_, err = runCLI(t, "", "agent", "set", "A-1", "role", "nope")
	if err == nil || !strings.Contains(err.Error(), "valid:") {
		t.Fatalf("bad role err: %v", err)
	}
}

func TestAgentGet(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/sessions/A-1": `{"id":"A-1","permission_mode":"plan","role":"reviewer","auto_approve":true,"force_compact":false}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "agent", "get", "A-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"permission-mode", "plan", "reviewer", "on", "off"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	out, err = runCLI(t, addr, "agent", "get", "A-1", "role")
	if err != nil || strings.TrimSpace(out) != "reviewer" {
		t.Fatalf("get role: %q %v", out, err)
	}
	out, err = runCLI(t, addr, "agent", "get", "A-1", "--json")
	if err != nil || !strings.Contains(out, `"compact": "off"`) {
		t.Fatalf("get json: %q %v", out, err)
	}
	if _, err = runCLI(t, addr, "agent", "get", "A-1", "bogus"); err == nil {
		t.Fatal("expected unknown-key error")
	}
}

func TestAgentSettingGroupsHiddenButInHelpAll(t *testing.T) {
	root := newRootCmd()
	for _, p := range []string{"agent permission-mode", "agent compact", "agent role set", "agent role set-tier"} {
		if c := findExactCommand(t, root, p); !c.Hidden {
			t.Errorf("%s should be hidden", p)
		}
	}
	for _, p := range []string{"agent set", "agent get", "agent role tier set"} {
		if c := findExactCommand(t, root, p); c.Hidden {
			t.Errorf("%s should be visible", p)
		}
	}
	out, err := runCLI(t, "", "help", "agent")
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"permission-mode", "compact"} {
		if strings.Contains(out, hidden+" ") && strings.Contains(out, "  "+hidden) {
			t.Errorf("%q listed in agent help:\n%s", hidden, out)
		}
	}
	all, err := runCLI(t, "", "help", "--all")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"agent permission-mode", "agent compact"} {
		if !strings.Contains(all, want) {
			t.Errorf("help --all missing %q", want)
		}
	}
}

func TestRoleTierSetCanonicalAndAlias(t *testing.T) {
	for _, args := range [][]string{
		{"agent", "role", "tier", "set", "worker", "tier-1"},
		{"agent", "role", "set-tier", "worker", "tier-1"},
	} {
		_ = withTestBackendStore(t)
		out, err := runGit(t, "127.0.0.1:0", args...)
		if err != nil || !strings.Contains(out, "default tier set to tier-1") {
			t.Fatalf("%v: %q %v", args, out, err)
		}
	}
}

func TestLegacySettingCommandsMarkedCompatibility(t *testing.T) {
	root := newRootCmd()
	for _, l := range []string{"set-permission-mode", "force-compact", "set-role"} {
		c := findExactCommand(t, root, l)
		if !c.Hidden || c.Annotations[AnnotationCanonicalPath] != "warden agent set" {
			t.Errorf("%s: hidden=%v canonical=%q", l, c.Hidden, c.Annotations[AnnotationCanonicalPath])
		}
	}
}
