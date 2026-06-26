package cli

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runCLIWithConfig is runCLI but with an explicit config file path (rather than a
// non-existent default), so a command that reads a config feature flag is
// exercised against real settings.
func runCLIWithConfig(t *testing.T, addr, configPath string, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	full := append([]string{}, args...)
	if addr != "" {
		full = append(full, "--addr", addr)
	}
	full = append(full, "--config", configPath)
	root.SetArgs(full)
	err := root.Execute()
	return out.String(), err
}

func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func TestInsightsCmdDisabled(t *testing.T) {
	// With insights: false the command must refuse before any daemon round-trip.
	cfg := writeConfig(t, "insights: false\n")
	called := false
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{}`))
	})
	_, err := runCLIWithConfig(t, addr, cfg, "insights")
	if err == nil {
		t.Fatal("insights must error when the feature is disabled")
	}
	if called {
		t.Fatal("a disabled feature must not contact the daemon")
	}
}

func TestInsightsCmdEnabled(t *testing.T) {
	cfg := writeConfig(t, "insights: true\n")
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/sessions":
			_, _ = w.Write([]byte(`{"sessions":[]}`))
		case "/history":
			_, _ = w.Write([]byte(`{"sessions":[{"id":"A-1","type":"development","status":"done"}]}`))
		case "/metrics/history":
			_, _ = w.Write([]byte(`{"summaries":[]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	})
	out, err := runCLIWithConfig(t, addr, cfg, "insights")
	if err != nil {
		t.Fatalf("insights: %v", err)
	}
	if !strings.Contains(out, "sessions analyzed") {
		t.Fatalf("insights output: %q", out)
	}
}

func TestInsightsCmdJSON(t *testing.T) {
	cfg := writeConfig(t, "insights: true\n")
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/sessions":
			_, _ = w.Write([]byte(`{"sessions":[]}`))
		case "/history":
			_, _ = w.Write([]byte(`{"sessions":[]}`))
		case "/metrics/history":
			_, _ = w.Write([]byte(`{"summaries":[]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	})
	out, err := runCLIWithConfig(t, addr, cfg, "insights", "--json")
	if err != nil {
		t.Fatalf("insights --json: %v", err)
	}
	if !strings.Contains(out, "{") {
		t.Fatalf("insights --json should emit a JSON object: %q", out)
	}
}

func TestOrchCmdStartsWithoutLocalLLM(t *testing.T) {
	// Default config has local_llm off. The deterministic /commands are the
	// fallback, so orch now starts and notes the NL half is off instead of
	// refusing. /help needs neither a daemon nor a model.
	out, err := runCLIStdin(t, "", "/help\nexit\n", "orch")
	if err != nil {
		t.Fatalf("orch should start without local_llm, got %v", err)
	}
	if !strings.Contains(out, "natural-language mode is off") {
		t.Fatalf("expected the NL-off notice, got %q", out)
	}
}
