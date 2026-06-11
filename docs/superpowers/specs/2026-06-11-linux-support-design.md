# Linux Support Design

**Date:** 2026-06-11
**Status:** Approved

## Goal

Add first-class Linux support to warden. The Go binary already cross-compiles
and runs on Linux (goreleaser targets `linux/amd64` and `linux/arm64`). What's
missing is: a systemd user service for auto-start, `notify-send` desktop
notifications, and updated install scripts that auto-detect the OS.

## Scope

- OS-detection in `scripts/common.sh` (Option A: single file, function override)
- New `deploy/warden.service.template` (systemd unit)
- `notify-send` support in `internal/notify/notify.go`
- README documentation for Linux service setup

Not in scope: Windows support, containerised deployment, changes to the Go
daemon logic, changes to the TUI or web dashboard.

## Architecture

### OS Detection

`scripts/common.sh` detects the platform at source-time:

```bash
OS_PLATFORM="$(uname -s | tr '[:upper:]' '[:lower:]')"   # "darwin" | "linux"
```

On macOS, `SERVICE_CONFIG` points to the launchd plist path
(`~/Library/LaunchAgents/com.srajanpathak.warden.plist`). On Linux it points to
the systemd user unit (`~/.config/systemd/user/warden.service`).

All three scripts (`install.sh`, `reinstall.sh`, `uninstall.sh`) are unchanged
except the single `$PLIST` reference in `uninstall.sh` is replaced with
`$SERVICE_CONFIG`.

### Service Management

At the bottom of `common.sh`, a Linux block redefines the five service
functions:

| Function | Linux implementation |
|---|---|
| `render_plist` | Renders `deploy/warden.service.template` → `SERVICE_CONFIG` |
| `service_loaded` | `systemctl --user is-active --quiet warden` |
| `load_service` | `mkdir -p …/systemd/user && daemon-reload && enable --now warden` |
| `unload_service` | `systemctl --user disable --now warden` |
| `reload_service` | `daemon-reload && restart warden` |
| `restart_service` | `daemon-reload && restart warden` |

macOS launchctl functions are untouched above the Linux block.

### systemd Unit File (`deploy/warden.service.template`)

Template variables: `__BINARY__`, `__ADDR__`, `__HOME__` — same three as the
plist template.

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

`Restart=always` + `RestartSec=2` matches launchd `KeepAlive=true`.
`WantedBy=default.target` makes it a user service that starts at login.
Logs go to the same `/tmp/warden.daemon.{log,err}` paths as macOS for
consistency; users can also use `journalctl --user -u warden`.

### Notifications (`internal/notify/notify.go`)

`New()` extended to a three-way dispatch:

```
enabled=false          → logNotifier
enabled + darwin       → osaNotifier   (osascript, existing)
enabled + linux + PATH → notifySendNotifier (notify-send, new)
enabled + linux no PATH → logNotifier
```

`notifySendNotifier.Notify(title, body)` calls `notify-send title body`.
No quoting needed (separate args, unlike osascript). Failure is logged,
never propagated.

### README

The existing macOS service section is unchanged. A parallel Linux block is
added directly after it, covering `install.sh`, `reinstall.sh`,
`uninstall.sh`, log paths, and the `notify-send` dependency note.

## Files Changed

| File | Change |
|---|---|
| `scripts/common.sh` | Add OS detection; add Linux service function overrides at bottom |
| `scripts/uninstall.sh` | Replace `$PLIST` with `$SERVICE_CONFIG` |
| `deploy/warden.service.template` | New — systemd unit template |
| `internal/notify/notify.go` | Add `notifySendNotifier`; update `New()` |
| `internal/notify/notify_test.go` | Add Linux notifier tests |
| `README.md` | Add Linux service setup section |

## Error Handling

- `render_plist` on Linux: same pattern as macOS — write to a `.tmp` file, `cmp` against existing, `mv` atomically; first error calls `die`.
- `load_service` on Linux: die on `systemctl` failure (mirrors `die "failed to load service"` on macOS).
- `notify-send` not on PATH: silently falls back to `logNotifier` (checked once at `New()` time via `exec.LookPath`).
- Unsupported OS (not darwin/linux): `die "unsupported platform: $OS_PLATFORM"` in `common.sh`.

## Testing

- `notify_test.go`: existing darwin tests keep passing; new Linux tests mock `run` to verify `notify-send title body` args and error logging.
- Manual smoke test: `make install` on Linux, verify `systemctl --user is-active warden`, verify `warden doctor` passes.
