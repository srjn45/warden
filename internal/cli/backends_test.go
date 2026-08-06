package cli

import (
	"strings"
	"testing"
)

// backendsStateJSON is a canned GET/rescan/default response: two CLI backends
// plus the reserved local row, with a non-default thinking mode.
const backendsStateJSON = `{
  "backends": [
    {"id":"claude","installed":true,"tier":"subscription","default":true,"enabled":true,"is_local":false},
    {"id":"codex","installed":true,"tier":"unclassified","default":false,"enabled":false,"is_local":false},
    {"id":"local","installed":true,"tier":"local","default":false,"enabled":true,"is_local":true}
  ],
  "settings": {"id":"__settings__","internal_thinking_mode":"local_only","allow_paid_autopilot":false}
}`

func TestBackendsListCmd(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/backends": backendsStateJSON,
	}, nil, nil))
	out, err := runCLI(t, addr, "backends", "list")
	if err != nil {
		t.Fatalf("backends list: %v", err)
	}
	for _, want := range []string{"ID", "INSTALLED", "TIER", "DEFAULT", "ENABLED", "LIMITED",
		"claude", "codex", "local", "internal thinking mode: local_only"} {
		if !strings.Contains(out, want) {
			t.Fatalf("backends list missing %q: %q", want, out)
		}
	}
}

func TestBackendsRescanCmd(t *testing.T) {
	method := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/backends/rescan": backendsStateJSON,
	}, method, nil))
	out, err := runCLI(t, addr, "backends", "rescan")
	if err != nil {
		t.Fatalf("backends rescan: %v", err)
	}
	if method["/api/v1/backends/rescan"] != "POST" {
		t.Fatalf("rescan not POSTed: %q", method["/api/v1/backends/rescan"])
	}
	if !strings.Contains(out, "local") {
		t.Fatalf("rescan output missing local row: %q", out)
	}
}

func TestBackendsTierCmd(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"PATCH /api/v1/backends/codex": `{"id":"codex","tier":"free","enabled":true}`,
	}, nil, body))
	out, err := runCLI(t, addr, "backends", "tier", "codex", "free")
	if err != nil {
		t.Fatalf("backends tier: %v", err)
	}
	if !strings.Contains(out, "backend codex tiered as free") {
		t.Fatalf("tier output: %q", out)
	}
	if !strings.Contains(body["/api/v1/backends/codex"], `"tier":"free"`) {
		t.Fatalf("tier not forwarded: %q", body["/api/v1/backends/codex"])
	}
}

func TestBackendsTierCmdInvalid(t *testing.T) {
	// A bad tier is rejected client-side before any daemon call.
	if _, err := runCLI(t, "", "backends", "tier", "codex", "nonsense"); err == nil {
		t.Fatal("expected an error for an invalid tier")
	}
	// The reserved local tier is not user-assignable.
	if _, err := runCLI(t, "", "backends", "tier", "codex", "local"); err == nil {
		t.Fatal("expected an error for the reserved local tier")
	}
}

func TestBackendsDefaultCmd(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"PUT /api/v1/backends/default": backendsStateJSON,
	}, nil, body))
	out, err := runCLI(t, addr, "backends", "default", "claude")
	if err != nil {
		t.Fatalf("backends default: %v", err)
	}
	if !strings.Contains(out, "default backend set to claude") {
		t.Fatalf("default output: %q", out)
	}
	if !strings.Contains(body["/api/v1/backends/default"], `"id":"claude"`) {
		t.Fatalf("default not forwarded: %q", body["/api/v1/backends/default"])
	}
}

func TestBackendsEnableDisableCmd(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"PATCH /api/v1/backends/aider": `{"id":"aider","enabled":true}`,
	}, nil, body))
	out, err := runCLI(t, addr, "backends", "enable", "aider")
	if err != nil {
		t.Fatalf("backends enable: %v", err)
	}
	if !strings.Contains(out, "backend aider enabled") {
		t.Fatalf("enable output: %q", out)
	}
	if !strings.Contains(body["/api/v1/backends/aider"], `"enabled":true`) {
		t.Fatalf("enable not forwarded: %q", body["/api/v1/backends/aider"])
	}

	body2 := map[string]string{}
	addr2 := stubDaemon(t, routedDaemon(t, map[string]string{
		"PATCH /api/v1/backends/aider": `{"id":"aider","enabled":false}`,
	}, nil, body2))
	out2, err := runCLI(t, addr2, "backends", "disable", "aider")
	if err != nil {
		t.Fatalf("backends disable: %v", err)
	}
	if !strings.Contains(out2, "backend aider disabled") {
		t.Fatalf("disable output: %q", out2)
	}
	if !strings.Contains(body2["/api/v1/backends/aider"], `"enabled":false`) {
		t.Fatalf("disable not forwarded: %q", body2["/api/v1/backends/aider"])
	}
}

func TestBackendsThinkingModeCmd(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"PUT /api/v1/backends/thinking-mode": `{"id":"__settings__","internal_thinking_mode":"local_only"}`,
	}, nil, body))
	out, err := runCLI(t, addr, "backends", "thinking-mode", "local_only")
	if err != nil {
		t.Fatalf("backends thinking-mode: %v", err)
	}
	if !strings.Contains(out, "internal thinking mode set to local_only") {
		t.Fatalf("thinking-mode output: %q", out)
	}
	if !strings.Contains(body["/api/v1/backends/thinking-mode"], `"mode":"local_only"`) {
		t.Fatalf("mode not forwarded: %q", body["/api/v1/backends/thinking-mode"])
	}
}

func TestBackendsThinkingModeCmdInvalid(t *testing.T) {
	if _, err := runCLI(t, "", "backends", "thinking-mode", "nonsense"); err == nil {
		t.Fatal("expected an error for an invalid thinking mode")
	}
}
