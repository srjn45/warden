// Package notify delivers short "an agent needs you" alerts to the user.
package notify

import (
	"fmt"
	"log"
	"os/exec"
	"runtime"
	"strconv"
)

// Notifier delivers a short attention message to the user.
type Notifier interface {
	Notify(title, body string)
}

// New returns the platform notifier: a macOS desktop notifier when enabled on
// darwin, else a log-only notifier (non-darwin, or notifications disabled).
func New(enabled bool) Notifier {
	if enabled && runtime.GOOS == "darwin" {
		return osaNotifier{run: execRun}
	}
	return logNotifier{}
}

func execRun(name string, args ...string) error { return exec.Command(name, args...).Run() }

// osaNotifier shows a macOS notification via osascript. Best-effort: a failure
// is logged, never propagated, so it can't disrupt the poll loop.
type osaNotifier struct {
	run func(name string, args ...string) error
}

func (o osaNotifier) Notify(title, body string) {
	// body/title become AppleScript string literals; strconv.Quote escapes the
	// quotes and newlines (subjects are short plain text, so this is sufficient).
	script := fmt.Sprintf("display notification %s with title %s", strconv.Quote(body), strconv.Quote(title))
	if err := o.run("osascript", "-e", script); err != nil {
		log.Printf("notify: osascript: %v", err)
	}
}

// logNotifier writes the notification to the log — the fallback when desktop
// notifications aren't available (non-darwin) or are disabled.
type logNotifier struct{}

func (logNotifier) Notify(title, body string) { log.Printf("notify: %s — %s", title, body) }
