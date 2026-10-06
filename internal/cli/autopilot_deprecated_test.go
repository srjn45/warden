package cli

import (
	"strings"
	"testing"
)

// The autopilot namespace has exactly status and land; every removed name
// errors with the replacement command.
func TestAutopilotRemovedCommandsHintReplacement(t *testing.T) {
	root := newRootCmd()
	ap := findExactCommand(t, root, "autopilot")
	var names []string
	for _, c := range ap.Commands() {
		names = append(names, c.Name())
	}
	if got := strings.Join(names, ","); got != "status,land" && got != "land,status" {
		t.Errorf("autopilot subcommands = %q, want status and land only", got)
	}

	want := map[string]string{
		"init":       "wd plan run <plan-id> --mode autopilot",
		"enable":     "wd plan run <plan-id> --mode autopilot",
		"on":         "wd plan run <plan-id> --mode autopilot",
		"register":   "wd plan run <plan-id> --mode autopilot",
		"start":      "wd plan run <plan-id> --mode autopilot",
		"disable":    "wd plan pause",
		"off":        "wd plan pause",
		"pause":      "wd plan pause",
		"resume":     "wd plan resume",
		"stop":       "wd plan stop",
		"unregister": "wd plan stop",
		"list":       "wd autopilot status",
		"run":        "wd autopilot status",
	}
	for name, repl := range want {
		out, err := runCLI(t, "http://127.0.0.1:1", "autopilot", name)
		if err == nil {
			t.Errorf("autopilot %s: expected error, got %q", name, out)
			continue
		}
		if !strings.Contains(err.Error(), "unknown command") || !strings.Contains(err.Error(), repl) {
			t.Errorf("autopilot %s error = %q, want hint containing %q", name, err, repl)
		}
	}
}
