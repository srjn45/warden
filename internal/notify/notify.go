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
// darwin, a notify-send notifier when enabled on linux (if notify-send is on
// PATH), else a log-only notifier.
func New(enabled bool) Notifier {
	return newWith(enabled, execRun, runtime.GOOS, exec.LookPath)
}

// newWith is the testable core of New. goos and lookPath are injected so tests
// can exercise every branch without depending on the host OS or PATH.
func newWith(enabled bool, run func(string, ...string) error, goos string, lookPath func(string) (string, error)) Notifier {
	if !enabled {
		return logNotifier{}
	}
	switch goos {
	case "darwin":
		return osaNotifier{run: run}
	case "linux":
		if _, err := lookPath("notify-send"); err == nil {
			return notifySendNotifier{run: run}
		}
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

// notifySendNotifier shows a desktop notification via notify-send (libnotify).
// Best-effort: failure is logged, never propagated.
type notifySendNotifier struct {
	run func(name string, args ...string) error
}

func (n notifySendNotifier) Notify(title, body string) {
	if err := n.run("notify-send", title, body); err != nil {
		log.Printf("notify: notify-send: %v", err)
	}
}

// logNotifier writes the notification to the log — the fallback when desktop
// notifications aren't available or are disabled.
type logNotifier struct{}

func (logNotifier) Notify(title, body string) { log.Printf("notify: %s — %s", title, body) }
