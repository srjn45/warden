// Command warden-notify is an example warden plugin (#47) in its recommended
// production shape: a single, statically-compiled binary that speaks warden's
// JSON-over-stdio hook protocol. Where post-commit-notifier/notifier.sh is the
// smallest possible shell exercise of the protocol, this is the next step up —
// typed request/response structs, per-event branching, and a genuinely useful
// side effect: it raises a desktop notification when one of your agents does
// something you'd want to look up from, most importantly a **failing check**.
//
// The contract (see internal/plugin/protocol.go) is intentionally tiny:
//
//	warden → stdin : one JSON Request per subscribed lifecycle event
//	plugin → stdout: one JSON Response (advisory only) — or nothing, to ack
//
// Everything here is fail-open by design, on BOTH sides. warden already treats a
// missing/slow/non-zero/garbage plugin as "log and skip" (a hook can never gate
// an agent), and this plugin mirrors that posture: if the OS notifier is absent
// or errors, it falls back to appending a line to a log file and still exits 0.
// So the worst case is a missed popup, never a stuck agent.
//
// Build & register:
//
//	go build -o warden-notify ./examples/plugins/desktop-notify
//	# then in ~/.warden/config.yaml:
//	# plugins:
//	#   enabled: true
//	#   registry:
//	#     - name: desktop-notify
//	#       path: /absolute/path/to/warden-notify
//	#       events: [post-check, post-commit, post-spawn]
//
// Stdlib only — no third-party imports — so `go build ./...` in this repo also
// compiles it, which is exactly how you'd want your own plugin gated in CI.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

// protocolVersion must match internal/plugin.ProtocolVersion. A plugin reads it
// to stay forward-compatible; warden records a mismatch but (fail-open) does not
// reject the response over it.
const protocolVersion = 1

// request mirrors plugin.Request — the JSON warden writes to our stdin. We only
// declare the fields we actually use; encoding/json ignores the rest, so this
// struct need not track every future payload key.
type request struct {
	ProtocolVersion int               `json:"protocol_version"`
	Event           string            `json:"event"`
	Session         session           `json:"session"`
	Payload         map[string]string `json:"payload"`
}

// session is the small, read-only projection of the agent's record warden hands
// a plugin (plugin.SessionMeta): enough to identify and locate the agent without
// exposing its full mutable state.
type session struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Repo     string `json:"repo"`
	Worktree string `json:"worktree"`
	Branch   string `json:"branch"`
	Workdir  string `json:"workdir"`
}

// response mirrors plugin.Response — advisory only. warden logs ok/message; it
// never changes warden's control flow. Emitting it is optional (exit 0 with no
// stdout is a valid silent ack), but a short message aids `wd` debug logs.
type response struct {
	ProtocolVersion int    `json:"protocol_version"`
	OK              bool   `json:"ok"`
	Message         string `json:"message,omitempty"`
}

func main() {
	// Read the whole request off stdin. warden closes stdin after writing, so a
	// bounded ReadAll is safe and simple.
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		// Nothing to act on — silent ack (exit 0). Never fail hard: a non-zero
		// exit just makes warden log us as failed, which is noise, not signal.
		return
	}
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return
	}

	title, body, notify := render(req)
	if !notify {
		// An event we subscribed to but don't surface (e.g. a passing check): ack
		// quietly so warden's debug log stays readable.
		reply(true, "ignored "+req.Event)
		return
	}

	if err := desktopNotify(title, body); err != nil {
		// Fall back to a log line so the signal isn't lost on headless boxes or
		// when no notifier binary is installed — then still report OK.
		logLine(title, body)
		reply(true, "logged (no desktop notifier: "+err.Error()+")")
		return
	}
	reply(true, "notified: "+title)
}

// render turns a hook request into a notification, and reports whether this
// event is worth surfacing at all. This is where a real plugin encodes its
// policy — here: always alert on a FAILED check (the high-value signal), and
// give a lighter heads-up on commits and spawns. Passing checks are dropped so
// you're not buried in green.
func render(req request) (title, body string, notify bool) {
	agent := req.Session.ID
	if agent == "" {
		agent = "agent"
	}
	switch req.Event {
	case "post-check":
		name := req.Payload["name"]
		passed, _ := strconv.ParseBool(req.Payload["passed"])
		if passed {
			return "", "", false // green is not news
		}
		return fmt.Sprintf("❌ check failed: %s", name),
			fmt.Sprintf("%s (%s) on %s", agent, req.Session.Type, branchOf(req)),
			true
	case "post-commit":
		if committed, _ := strconv.ParseBool(req.Payload["committed"]); !committed {
			return "", "", false // nothing was actually committed
		}
		sha := shortSHA(req.Payload["sha"])
		return fmt.Sprintf("✅ commit %s", sha),
			fmt.Sprintf("%s → %s", agent, req.Payload["branch"]),
			true
	case "post-spawn":
		return fmt.Sprintf("🚀 agent started: %s", agent),
			fmt.Sprintf("%s in %s", req.Session.Type, req.Session.Repo),
			true
	default:
		return "", "", false
	}
}

// desktopNotify raises an OS-native notification, best-effort and cross-platform.
// It returns an error (rather than panicking) so the caller can fall back to a
// log line — the whole point of the fail-open posture.
func desktopNotify(title, body string) error {
	switch runtime.GOOS {
	case "darwin":
		// AppleScript is the dependency-free path on macOS.
		script := fmt.Sprintf("display notification %q with title %q", body, title)
		return exec.Command("osascript", "-e", script).Run()
	case "linux":
		path, err := exec.LookPath("notify-send")
		if err != nil {
			return fmt.Errorf("notify-send not found")
		}
		return exec.Command(path, title, body).Run()
	default:
		return fmt.Errorf("unsupported OS %q", runtime.GOOS)
	}
}

// logLine is the universal fallback: append a timestamped line to a log file so
// the event is never silently dropped. Override the path with WARDEN_NOTIFY_LOG.
func logLine(title, body string) {
	path := os.Getenv("WARDEN_NOTIFY_LOG")
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".warden", "plugin-desktop-notify.log")
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s  %s — %s\n", time.Now().UTC().Format(time.RFC3339), title, body)
}

// reply writes the advisory response to stdout. Marshaling a fixed struct can't
// realistically fail; on the off chance it does we just stay silent (a valid ack).
func reply(ok bool, msg string) {
	out, err := json.Marshal(response{ProtocolVersion: protocolVersion, OK: ok, Message: msg})
	if err != nil {
		return
	}
	os.Stdout.Write(out)
}

func branchOf(req request) string {
	if b := req.Payload["branch"]; b != "" {
		return b
	}
	return req.Session.Branch
}

// shortSHA trims a full commit hash to the familiar 7-char form for display.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	if sha == "" {
		return "(none)"
	}
	return sha
}
