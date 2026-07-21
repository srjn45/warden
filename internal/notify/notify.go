// Package notify delivers short "an agent needs you" alerts to the user.
package notify

import (
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
)

// Notifier delivers a short attention message to the user.
type Notifier interface {
	Notify(title, body string)
}

// Switch is a Notifier whose backing notifier can be swapped atomically at
// runtime. The daemon wires every alert hook (status transitions, context
// alerts, autopilot escalations, branch-tracker CI failures) to a single Switch
// once at startup; a config reload then rebuilds the desktop/webhook delivery
// chain (notify.enabled, notify.webhook.*) and installs it via Set WITHOUT having
// to re-wire the many closures that already captured the notifier. Safe for
// concurrent Notify and Set.
type Switch struct {
	mu    sync.RWMutex
	inner Notifier
}

// NewSwitch wraps inner in a swappable Switch. A nil inner degrades to log-only.
func NewSwitch(inner Notifier) *Switch {
	if inner == nil {
		inner = logNotifier{}
	}
	return &Switch{inner: inner}
}

// Set atomically swaps the backing notifier (config reload). A nil replacement
// degrades to log-only so the Switch always has a live delivery target.
func (s *Switch) Set(inner Notifier) {
	if inner == nil {
		inner = logNotifier{}
	}
	s.mu.Lock()
	s.inner = inner
	s.mu.Unlock()
}

// Notify forwards to the current backing notifier under the read lock.
func (s *Switch) Notify(title, body string) {
	s.mu.RLock()
	n := s.inner
	s.mu.RUnlock()
	n.Notify(title, body)
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
		slog.Warn("notify: osascript failed", "err", err)
	}
}

// notifySendNotifier shows a desktop notification via notify-send (libnotify).
// Best-effort: failure is logged, never propagated.
type notifySendNotifier struct {
	run func(name string, args ...string) error
}

func (n notifySendNotifier) Notify(title, body string) {
	if err := n.run("notify-send", title, body); err != nil {
		slog.Warn("notify: notify-send failed", "err", err)
	}
}

// logNotifier writes the notification to the log — the fallback when desktop
// notifications aren't available or are disabled.
type logNotifier struct{}

func (logNotifier) Notify(title, body string) { slog.Info("notify", "title", title, "body", body) }
