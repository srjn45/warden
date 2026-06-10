# Warden Linux Support — Design Spec

**Date:** 2026-06-10  
**Status:** Future enhancement — high-level design. Detail pass required before implementation.

---

## Summary

Make warden run natively on Linux (Ubuntu/Debian primary target, distro-agnostic where possible). Most of the stack already works on Linux — tmux, Go, ps, the web UI. The gaps are macOS-specific syscalls in metrics collection and the launchd-based daemon management.

---

## What Already Works on Linux

- Go daemon binary (no OS-specific imports outside metrics + codesign)
- tmux session management
- `ps -axo pid=,ppid=,rss=,pcpu=,etime=` — POSIX-compatible, works on Linux
- Web UI (static build, served from daemon)
- CLI (`warden` binary)
- Git worktree operations
- Transcript parsing, digest, approvals, pipelines

---

## What Needs to Change

### 1. Metrics Collection (`internal/metrics/collect.go`)

All macOS-specific shell-outs need Linux equivalents:

| macOS | Linux replacement |
|---|---|
| `vm_stat` | Parse `/proc/meminfo` (MemTotal, MemAvailable, MemFree, Cached, SwapTotal, SwapFree) |
| `sysctl -n hw.memsize` | `MemTotal` from `/proc/meminfo` |
| `sysctl -n vm.swapusage` | `SwapTotal` / `SwapFree` from `/proc/meminfo` |
| Memory pressure level (`normal`/`warn`/`critical`) | `/proc/pressure/memory` PSI (available Linux 4.20+; fallback: derive from MemAvailable% if PSI absent) |
| `lsof` for open FD count | Count entries in `/proc/self/fd/` |

**Implementation approach: build-tagged files**

```
internal/metrics/collect_darwin.go   // +build darwin
internal/metrics/collect_linux.go    // +build linux
```

Shared `Collector` struct and `Sample()` interface stay in `collect.go` (no build tag). Platform files implement `sampleSystem()` and `sampleFDs()`. Pure `/proc` parsers in `collect_linux.go` are unit-testable without shelling out.

### 2. Daemon Management

| macOS | Linux |
|---|---|
| `scripts/com.srajanpathak.warden.plist` (launchd) | `scripts/warden.service` (systemd user unit) |
| `launchctl load/unload` | `systemctl --user enable/start/stop` |
| `reinstall.sh` uses `launchctl` | Detect `GOOS` → use systemd path |

Systemd unit template:
```ini
[Unit]
Description=Warden agent orchestrator daemon
After=network.target

[Service]
ExecStart=%h/.local/bin/warden daemon
Restart=on-failure
Environment=WARDEN_DATA_DIR=%h/.warden

[Install]
WantedBy=default.target
```

`make install` detects OS via `$(shell uname)` and runs the appropriate install path.

### 3. Codesigning

macOS-only. Wrap in a build tag or `if runtime.GOOS == "darwin"` guard. On Linux, codesigning is a no-op — `scripts/common.sh` skips the sign/verify steps.

### 4. `internal/pressure` Package

Uses `sysctl` on macOS. Add a Linux path reading `/proc/pressure/memory` (PSI). Same build-tag split as metrics.

---

## Testing Strategy

- `collect_linux.go` parsers are pure functions over strings (same pattern as `collect_darwin.go` test fakes) — fully unit-testable without a Linux machine
- Integration: add `ubuntu-latest` runner to GitHub Actions CI
- The existing `fakeRunner` in `collect_test.go` already abstracts shell-outs — Linux tests use the same approach with Linux-format output strings

---

## Distribution Support

**Primary:** Ubuntu 22.04+ / Debian 12+ (systemd, kernels that have `/proc/pressure`)

**Secondary / best-effort:**
- Arch, Fedora, other systemd distros
- Alpine / Docker (no systemd — document running daemon directly without a service manager, `warden daemon &`)

PSI (`/proc/pressure/memory`) requires kernel ≥ 4.20 and CONFIG_PSI=y. Fallback: if `/proc/pressure/memory` is absent, derive pressure level from `MemAvailable/MemTotal`: <20% → critical, <40% → warn, else normal.

---

## Release / CI Changes

- `goreleaser` already produces linux/amd64 + linux/arm64 binaries (check `.goreleaser.yaml`)
- Add `ubuntu-latest` to the test matrix in `.github/workflows/`
- Add a smoke test job: install the binary, run `warden doctor` (or equivalent self-check), confirm exit 0

---

## Open Questions for Detail Pass

1. Does `internal/pressure` need the same build-tag split, or can it share logic with metrics collect?
2. cgroups v2 — should per-agent RSS attribution use cgroup stats on Linux instead of `ps` tree walk? More accurate for containerized agents. Probably v2 enhancement.
3. macOS Notification Center → Linux equivalent for desktop notifications? (`notify-send` via D-Bus). Currently `internal/notify` uses macOS APIs.
4. Does `warden doctor` need OS-specific checks (e.g., verify `/proc/pressure` available, systemd unit loaded)?
