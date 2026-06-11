# Linux Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add first-class Linux support: systemd user service management, `notify-send` desktop notifications, and updated install scripts that auto-detect the OS.

**Architecture:** OS detection lives entirely in `scripts/common.sh` via `uname -s`; a Linux block at the bottom overrides the five service-management functions with `systemctl --user` equivalents. The Go binary gains a `notifySendNotifier` type selected by `New()` when `runtime.GOOS == "linux"` and `notify-send` is on PATH. All other Go code and the three shell scripts (`install.sh`, `reinstall.sh`, `uninstall.sh`) need minimal or no changes.

**Tech Stack:** Go 1.26, Bash, systemd (user services), `notify-send` (libnotify)

---

## File Map

| File | Action | Responsibility |
|---|---|---|
| `internal/notify/notify.go` | Modify | Add `notifySendNotifier`; update `New()` and extract `newWith()` for testability |
| `internal/notify/notify_test.go` | Modify | Add Linux notifier tests; update `TestNewSelectsByPlatformAndEnabled` |
| `deploy/warden.service.template` | Create | systemd user unit template (substitutes `__BINARY__`, `__ADDR__`, `__HOME__`) |
| `scripts/common.sh` | Modify | Add OS detection; add Linux service function overrides at bottom; introduce `SERVICE_CONFIG` |
| `scripts/uninstall.sh` | Modify | Replace `$PLIST` with `$SERVICE_CONFIG` |
| `README.md` | Modify | Add Linux service setup section after the existing macOS section |

---

## Task 1: Linux `notify-send` support

**Files:**
- Modify: `internal/notify/notify.go`
- Modify: `internal/notify/notify_test.go`

- [ ] **Step 1.1: Write the failing tests**

Add to `internal/notify/notify_test.go`, inside `package notify`:

```go
func TestNotifySendNotifierBuildsArgs(t *testing.T) {
	var gotName string
	var gotArgs []string
	n := notifySendNotifier{run: func(name string, args ...string) error {
		gotName = name
		gotArgs = args
		return nil
	}}
	n.Notify("warden — needs input", "agent-a1b2: review auth")
	require.Equal(t, "notify-send", gotName)
	require.Equal(t, []string{"warden — needs input", "agent-a1b2: review auth"}, gotArgs)
}

func TestNotifySendNotifierLogsError(t *testing.T) {
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)
	n := notifySendNotifier{run: func(name string, args ...string) error {
		return fmt.Errorf("mock failure")
	}}
	n.Notify("title", "body") // must not panic or propagate error
	require.Contains(t, logBuf.String(), "notify-send")
}

func TestNewWithLinux(t *testing.T) {
	// notify-send found → notifySendNotifier
	lookFound := func(string) (string, error) { return "/usr/bin/notify-send", nil }
	require.IsType(t, notifySendNotifier{}, newWith(true, execRun, "linux", lookFound))

	// notify-send not found → logNotifier
	lookMissing := func(string) (string, error) { return "", fmt.Errorf("not found") }
	require.IsType(t, logNotifier{}, newWith(true, execRun, "linux", lookMissing))

	// disabled → logNotifier regardless
	require.IsType(t, logNotifier{}, newWith(false, execRun, "linux", lookFound))
}

func TestNewWithDarwin(t *testing.T) {
	look := func(string) (string, error) { return "", nil } // unused on darwin
	require.IsType(t, osaNotifier{}, newWith(true, execRun, "darwin", look))
	require.IsType(t, logNotifier{}, newWith(false, execRun, "darwin", look))
}
```

Add the missing imports to the test file (add `bytes`, `fmt`, `log`, `os` — keep existing imports):

```go
import (
	"bytes"
	"fmt"
	"log"
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)
```

- [ ] **Step 1.2: Run tests to confirm they fail**

```bash
go test ./internal/notify/...
```

Expected: compile error — `notifySendNotifier`, `newWith` undefined.

- [ ] **Step 1.3: Implement `notifySendNotifier` and `newWith` in `notify.go`**

Replace the entire contents of `internal/notify/notify.go` with:

```go
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
```

- [ ] **Step 1.4: Update `TestNewSelectsByPlatformAndEnabled` in `notify_test.go`**

The existing test's `else` branch still asserts `logNotifier` for non-darwin, which would fail on Linux now that `New()` may return `notifySendNotifier`. Replace the whole `TestNewSelectsByPlatformAndEnabled` function with:

```go
func TestNewSelectsByPlatformAndEnabled(t *testing.T) {
	require.IsType(t, logNotifier{}, New(false), "disabled → log notifier")
	// New() on darwin always picks osaNotifier; on linux it depends on PATH.
	// The newWith tests cover the branching exhaustively — this just checks the
	// public constructor forwards enabled=false correctly on any platform.
}
```

- [ ] **Step 1.5: Run tests to confirm they pass**

```bash
go test ./internal/notify/...
```

Expected: `ok github.com/srjn45/warden/internal/notify`

- [ ] **Step 1.6: Commit**

```bash
git add internal/notify/notify.go internal/notify/notify_test.go
git commit -m "feat(notify): add notify-send support for Linux"
```

---

## Task 2: systemd unit template

**Files:**
- Create: `deploy/warden.service.template`

- [ ] **Step 2.1: Create `deploy/warden.service.template`**

```ini
[Unit]
Description=warden agent daemon
After=network.target

[Service]
Type=simple
ExecStart=__BINARY__ daemon
Environment=WARDEN_ADDR=__ADDR__
Environment=WARDEN_SPAWN_GATE_MAX_AGENTS=10
Environment=PATH=__HOME__/.local/bin:/usr/local/bin:/usr/bin:/bin
Restart=always
RestartSec=2
StandardOutput=append:/tmp/warden.daemon.log
StandardError=append:/tmp/warden.daemon.err

[Install]
WantedBy=default.target
```

- [ ] **Step 2.2: Verify the template renders cleanly by hand (optional smoke test)**

```bash
sed -e "s|__BINARY__|$HOME/.local/bin/warden|g" \
    -e "s|__ADDR__|127.0.0.1:8765|g" \
    -e "s|__HOME__|$HOME|g" \
    deploy/warden.service.template
```

Expected: valid-looking INI with no remaining `__` placeholders.

- [ ] **Step 2.3: Commit**

```bash
git add deploy/warden.service.template
git commit -m "feat(deploy): add systemd user service template for Linux"
```

---

## Task 3: OS detection and Linux service functions in `common.sh`

**Files:**
- Modify: `scripts/common.sh`

- [ ] **Step 3.1: Add OS detection and `SERVICE_CONFIG` after the existing config block**

In `scripts/common.sh`, the existing config block ends at the line:

```bash
TEMPLATE="$REPO_ROOT/deploy/$LABEL.plist.template"
```

Insert immediately after that line:

```bash
# Detect platform; SERVICE_CONFIG is the canonical service-config path for all
# scripts — plist on macOS, systemd unit on Linux.
OS_PLATFORM="$(uname -s | tr '[:upper:]' '[:lower:]')"
SERVICE_CONFIG="$PLIST"   # default: macOS plist; overridden for Linux below
```

- [ ] **Step 3.2: Add the Linux override block at the very bottom of `common.sh`**

Append after the last existing function (`check_path`):

```bash
# --- Linux overrides (systemd user service) --------------------------------
# These five functions replace their launchctl counterparts above when running
# on Linux. All other helpers (build_release, deploy_binary, codesign_binary,
# report_health, check_path) are already platform-safe.
if [ "$OS_PLATFORM" = "linux" ]; then
  SERVICE_CONFIG="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/warden.service"
  TEMPLATE="$REPO_ROOT/deploy/warden.service.template"

  render_plist() {
    [ -f "$TEMPLATE" ] || die "service template not found: $TEMPLATE"
    mkdir -p "$(dirname "$SERVICE_CONFIG")"
    local tmp="$SERVICE_CONFIG.tmp.$$"
    sed -e "s|__BINARY__|$INSTALL_BIN|g" \
        -e "s|__ADDR__|$ADDR|g" \
        -e "s|__HOME__|$HOME|g" \
        "$TEMPLATE" > "$tmp" || { rm -f "$tmp"; die "failed to render service file"; }
    if [ -f "$SERVICE_CONFIG" ] && cmp -s "$tmp" "$SERVICE_CONFIG"; then
      rm -f "$tmp"
      PLIST_CHANGED=0
      info "service file unchanged: $SERVICE_CONFIG"
    else
      mv -f "$tmp" "$SERVICE_CONFIG"
      PLIST_CHANGED=1
      info "wrote $SERVICE_CONFIG"
    fi
  }

  service_loaded() {
    systemctl --user is-active --quiet warden 2>/dev/null
  }

  load_service() {
    mkdir -p "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
    systemctl --user daemon-reload || die "systemctl daemon-reload failed"
    if systemctl --user enable --now warden 2>/dev/null; then
      info "service enabled and started"
    else
      die "failed to enable/start warden service"
    fi
  }

  unload_service() {
    if ! service_loaded; then
      info "service not active; nothing to stop"
      return 0
    fi
    if systemctl --user disable --now warden 2>/dev/null; then
      info "service disabled and stopped"
    else
      warn "could not stop service (it may already be gone)"
    fi
  }

  reload_service() {
    systemctl --user daemon-reload || die "systemctl daemon-reload failed"
    systemctl --user restart warden || die "failed to restart warden service"
    info "service restarted"
  }

  restart_service() {
    systemctl --user daemon-reload || die "systemctl daemon-reload failed"
    if service_loaded; then
      systemctl --user restart warden || die "failed to restart warden service"
      info "service restarted"
    else
      load_service
    fi
  }
fi
```

- [ ] **Step 3.3: Verify the macOS path still looks right (no unintended changes)**

```bash
bash -n scripts/common.sh
```

Expected: no output (clean syntax check).

- [ ] **Step 3.4: Commit**

```bash
git add scripts/common.sh
git commit -m "feat(scripts): add Linux systemd service management to common.sh"
```

---

## Task 4: Fix `$PLIST` reference in `uninstall.sh`

**Files:**
- Modify: `scripts/uninstall.sh`

- [ ] **Step 4.1: Replace `$PLIST` with `$SERVICE_CONFIG` in the file-removal block**

In `scripts/uninstall.sh`, find the block:

```bash
if [ -f "$PLIST" ]; then
  rm -f "$PLIST"
  info "removed $PLIST"
fi
```

Replace it with:

```bash
if [ -f "$SERVICE_CONFIG" ]; then
  rm -f "$SERVICE_CONFIG"
  info "removed $SERVICE_CONFIG"
fi
```

- [ ] **Step 4.2: Syntax check**

```bash
bash -n scripts/uninstall.sh
```

Expected: no output.

- [ ] **Step 4.3: Commit**

```bash
git add scripts/uninstall.sh
git commit -m "fix(scripts): use \$SERVICE_CONFIG in uninstall.sh for cross-platform cleanup"
```

---

## Task 5: README Linux service documentation

**Files:**
- Modify: `README.md`

- [ ] **Step 5.1: Add Linux service section to `README.md`**

In `README.md`, locate the end of the macOS service section. It ends just before:

```markdown
## Wire in the Claude Code hooks
```

Insert the following block immediately before that heading (after the macOS section ends):

```markdown
### Run it as a background service (Linux — systemd)

The same install script detects Linux and registers a systemd **user service**
instead of a launchd plist. It builds the release, installs the binary to
`~/.local/bin/warden`, writes
`~/.config/systemd/user/warden.service`, enables it, and links the Claude skill
and MCP server:

```sh
./scripts/install.sh        # or: make install
```

The daemon starts automatically at each login session and restarts on crash
(`Restart=always`), listening on `127.0.0.1:8765` by default.

> `~/.local/bin` must be on your `PATH` — the installer warns if it isn't.

**Redeploy after a code change:**

```sh
./scripts/reinstall.sh             # rebuild UI + binary, redeploy, restart
./scripts/reinstall.sh --no-build  # redeploy existing build only
# or: make reinstall  /  make reinstall NO_BUILD=1
```

**Uninstall** (stops and removes the service, binary, skill link, and MCP
registration; **preserves** `~/.warden` and logs):

```sh
./scripts/uninstall.sh
./scripts/uninstall.sh --keep-binary   # leave ~/.local/bin/warden in place
```

Logs:
- stdout: `/tmp/warden.daemon.log`
- stderr: `/tmp/warden.daemon.err`
- or live: `journalctl --user -u warden -f`

> **Notifications:** off by default. Set `WARDEN_NOTIFY=on` to enable. The
> daemon calls `notify-send` (libnotify) when it's on `PATH`; install it with
> `apt install libnotify-bin` (Debian/Ubuntu) or `dnf install libnotify`
> (Fedora). Degrades to log-only if `notify-send` is not found.

---
```

- [ ] **Step 5.2: Commit**

```bash
git add README.md
git commit -m "docs: add Linux systemd service setup to README"
```

---

## Task 6: Full verification

- [ ] **Step 6.1: Run the full Go test suite**

```bash
go test ./...
```

Expected: all packages pass, no failures.

- [ ] **Step 6.2: Build a Linux binary and smoke-test it**

```bash
GOOS=linux GOARCH=amd64 go build -o /tmp/warden-linux ./cmd/warden
file /tmp/warden-linux
```

Expected: `ELF 64-bit LSB executable` (or similar confirming Linux binary).

```bash
/tmp/warden-linux --help 2>&1 | head -5
```

Expected: warden usage output (runs via binfmt_misc or natively on Linux).

- [ ] **Step 6.3: Verify template renders without placeholders**

```bash
bash -c '
  source scripts/common.sh
  render_plist 2>/dev/null || true
  grep "__" "${SERVICE_CONFIG:-/dev/null}" 2>/dev/null && echo "FAIL: placeholders remain" || echo "ok"
'
```

On macOS this will render the plist; on Linux it renders the systemd unit. Either way, no `__` tokens should remain.

- [ ] **Step 6.4: Lint the shell scripts**

```bash
bash -n scripts/common.sh scripts/install.sh scripts/reinstall.sh scripts/uninstall.sh
```

Expected: no output.

- [ ] **Step 6.5: Final commit if any loose files remain**

```bash
git status
```

If clean, nothing to do. If not:

```bash
git add -p   # stage only intended files
git commit -m "chore: linux support cleanup"
```
