# Shipping warden

This guide describes how to take warden from its current **single-developer,
local-macOS** setup to something other people can install on macOS *and* Linux.
It documents the real flow as it exists today, the concrete blockers to wider
distribution, and a step-by-step plan to remove them.

> Scope: this is a planning/reference document. None of the changes below are
> implemented yet — each section calls out what exists vs. what needs building.

---

## 1. How distribution works today

### Build: `make release`

```make
release: ui build
```

Two steps, in order:

1. **`ui`** — `cd web && npm ci && npm run build`, producing `web/dist`.
2. **`build`** — `go build -o bin/warden ./cmd/warden`.

The ordering matters: the Go binary embeds the web UI via `go:embed`, so the UI
must be built *first* or the embed picks up stale/empty assets. The result is a
**single self-contained binary** at `bin/warden` that serves the web GUI, the
HTTP daemon API, the MCP server, the TUI, and the CLI.

### Install: `scripts/install.sh`

`make install` calls `scripts/install.sh`, which (sourcing `scripts/common.sh`):

1. `build_release` — runs `make release` (skip with `NO_BUILD=1` / `--no-build`).
2. `deploy_binary` — copies `bin/warden` → `~/.local/bin/warden`, then
   **code-signs** it with a stable self-signed identity (`warden-codesign`,
   created once by `scripts/codesign-setup.sh`) so a granted macOS Full Disk
   Access survives rebuilds.
3. `render_plist` — renders `deploy/com.srajanpathak.warden.plist.template`
   by `sed`-substituting `__BINARY__`, `__ADDR__`, `__HOME__`, writing it to
   `~/Library/LaunchAgents/com.srajanpathak.warden.plist`.
4. `restart_service` — boots the launchd job (`launchctl bootstrap`, falling
   back to legacy `load -w`). Re-derives launchd's Lightweight Code Requirement
   on binary/plist change to avoid the stale-LWCR spawn failure (EX_CONFIG/78).
5. **Skill symlink** — `ln -sfn $REPO/skills/warden ~/.claude/skills/warden`.
6. **MCP registration** — `claude mcp add warden --scope user -- warden mcp`
   (idempotent remove-then-add; degrades to a warning if enterprise MCP policy
   blocks it).
7. `check_path` / `report_health` — warns if `~/.local/bin` isn't on `PATH`,
   then polls `/healthz` until the daemon answers.

Companion scripts: `scripts/reinstall.sh` (rebuild + redeploy the running
daemon) and `scripts/uninstall.sh` (bootout, remove plist/binary/skill-symlink/
MCP registration; **preserves** `~/.warden` data and `/tmp/warden.daemon.*`
logs).

### Runtime layout

| Thing | Location |
|---|---|
| Binary | `~/.local/bin/warden` |
| launchd job | `~/Library/LaunchAgents/com.srajanpathak.warden.plist` |
| Daemon address | `127.0.0.1:8765` (`WARDEN_ADDR`) |
| Session store | `~/.warden/` |
| Logs | `/tmp/warden.daemon.log`, `/tmp/warden.daemon.err` |
| Claude skill | `~/.claude/skills/warden` → repo (symlink) |
| Claude hook | repo `hooks/warden-hook.sh`, wired via `settings.snippet.json` |

---

## 2. Blockers to wider distribution

The current flow assumes **one specific machine and one specific user**. The
hard dependencies on the source checkout and on a single person's identifiers
are what stop a stranger from installing this cleanly.

1. **No version embedding.** `go build` bakes in no version; the root cobra
   command (`internal/cli/root.go`) sets no `Version` field, and there is no
   `warden --version` or `warden doctor`. You cannot tell which build is
   running or triage a broken install in the field.

2. **Hardcoded launchd label.** `com.srajanpathak.warden` is wired into
   `scripts/common.sh` (`LABEL=`), the plist template filename, and the plist
   `<Label>`. It carries one person's name and is not parameterizable.

3. **Skill is symlinked into the repo.** `~/.claude/skills/warden` is a
   *symlink back to the source tree*. Delete or move the checkout and the
   installed tool's skill breaks. A distributed install must own a real copy.

4. **Hook path must be rendered per checkout.** `hooks/settings.snippet.json`
   uses a `__WARDEN_HOOK__` placeholder that the user substitutes with the
   absolute path to their clone's `hooks/warden-hook.sh` (the README shows a
   `sed` one-liner). It is still pasted into the user's Claude `settings.json`
   by hand — there is no installer step for it.

5. **macOS-only service management.** Everything service-related is launchd
   (`launchctl bootstrap/bootout/kickstart`, `.plist`). There is no Linux
   equivalent, so the daemon cannot run as a managed service on Linux.

6. **Single-platform build.** `make release` builds only for the host. There is
   no multi-arch / multi-OS release pipeline and no published artifacts.

7. **No published channel.** Installation requires cloning the repo and running
   `make`. There is no `brew install`, no release tarball, no `go install`-able
   tagged version for non-developers.

---

## 3. Runtime prerequisites

warden shells out to several external tools at runtime. These are **not**
bundled in the binary and must be present on `PATH` for the daemon and agents to
function. The planned `warden doctor` (§4.1) should check each one.

| Tool | Why it's needed | Notes |
|---|---|---|
| **tmux** | Every agent runs inside a tmux session; the session name *is* the warden session id. Spawn, attach, send-keys, copy-mode scrolling, and the web/CLI attach all go through tmux. | Hard requirement. `brew install tmux` / `apt install tmux`. |
| **git** | Worktree-isolated agents, branch/numstat for the completion digest, repo detection. | Hard requirement. |
| **claude** | The Claude Code CLI is what each agent actually runs. Also used by the installer to register the MCP server and by the digest's `claude -p` narrator. | Hard requirement for spawning agents; MCP registration degrades to a warning if absent. |
| **gh** | GitHub CLI, used for PR/issue operations in agent workflows. | Recommended; needed for git-hosting tasks. |
| **curl** | Used by the Claude hook (`warden-hook.sh`) to POST events to the daemon, and by the installer's health probe. | Present by default on macOS and most Linux. |

Platform notes:

- macOS: a one-time `scripts/codesign-setup.sh` avoids repeated Full Disk Access
  prompts. Not relevant on Linux.
- Linux: tmux + a real `$TERM` matter — the daemon forces `TERM=xterm-256color`
  on attach PTYs because a service-managed process has no inherited `TERM`
  (same gotcha that bit the launchd daemon).

---

## 4. The plan

Ordered roughly by dependency: version/doctor and de-hardcoding first (they make
everything else debuggable and portable), then the release pipeline, then the
per-platform service units, then the user-facing channel.

### 4.1 Version embedding + `--version` / `doctor`

**Goal:** every binary knows what it is and can self-diagnose.

Add a small build-info package (e.g. `internal/buildinfo`) with exported vars set
at link time:

```go
package buildinfo

var (
	Version = "dev"     // set via ldflags: -X .../buildinfo.Version=v1.2.3
	Commit  = "none"
	Date    = "unknown"
)
```

Build with ldflags:

```sh
go build -ldflags "\
  -X github.com/srjn45/warden/internal/buildinfo.Version=$(git describe --tags --always) \
  -X github.com/srjn45/warden/internal/buildinfo.Commit=$(git rev-parse --short HEAD) \
  -X github.com/srjn45/warden/internal/buildinfo.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o bin/warden ./cmd/warden
```

Wire it into cobra in `internal/cli/root.go`:

```go
root := &cobra.Command{
	Use:     "warden",
	Version: buildinfo.Version, // gives `warden --version` for free
	// ...
}
```

Add a **`doctor`** subcommand that prints the build info and checks the runtime
contract, so a user can paste its output into a bug report:

- version / commit / build date;
- presence + version of `tmux`, `git`, `claude`, `gh`, `curl` (the §3 table);
- daemon reachability (`GET /healthz` at `WARDEN_ADDR`) and whether the
  service is loaded (launchd/systemd, per platform);
- install paths: binary location, session store, log files;
- whether the Claude skill and hook are installed and point at a real file.

`make release` and the GoReleaser config (§4.3) should both inject ldflags so
released binaries report a real version while `go build` locally stays `dev`.

### 4.2 De-hardcode the install

Three independent changes, all in the scripts + templates (no behavior change on
the author's machine, since the defaults stay the same):

**(a) Parameterize the launchd label.** Replace the literal in
`scripts/common.sh` with an overridable default and make the template generic:

```sh
LABEL="${WARDEN_LABEL:-com.warden.daemon}"
```

Rename the template to a placeholder name (e.g.
`deploy/warden.plist.template`) and render `<Label>` from `$LABEL` instead of
hardcoding it. Keep the install/uninstall/reinstall scripts deriving the plist
path from `$LABEL` (they already do). This drops the author's name from the
shipped artifact and lets a user pick their own label if they want.

**(b) Copy the skill instead of symlinking.** Change the install step (and
`make install-skill`) from `ln -sfn` to a real copy:

```sh
rm -rf "$HOME/.claude/skills/warden"
cp -R "$REPO_ROOT/skills/warden" "$HOME/.claude/skills/warden"
```

This decouples the installed tool from the source checkout — the whole point of
a distributed install. Update `uninstall.sh` accordingly: it currently only
removes the link if it points back into the repo; with a copy it should remove
the directory unconditionally (or guard with a marker file it wrote).

**(c) Auto-discover the hook path.** The hook should not reference a specific
checkout. Two viable approaches, in order of preference:

1. **Ship the hook as a subcommand of the binary.** Add `warden hook <EVENT>`
   that does what `warden-hook.sh` does today (read tmux `#S`, read stdin
   JSON, POST `/events`). Then `settings.snippet.json` becomes
   `"command": "warden hook SessionStart"` — no path at all, just relies on
   `warden` being on `PATH`. This is the cleanest: one artifact, no second
   file to install, version-locked to the binary.
2. **Install the script to a stable location** (`~/.local/bin/warden-hook.sh`)
   and have the installer render the snippet with that absolute path, merging it
   into the user's `~/.claude/settings.json` instead of asking them to paste it.

Either way, the installer should *own* hook installation rather than leaving it
as a manual copy-paste of a path that only exists on one machine.

### 4.3 GoReleaser config (multi-platform builds)

**Goal:** `git tag vX.Y.Z && git push --tags` produces signed, versioned
archives for macOS (arm64 + amd64) and Linux (arm64 + amd64), plus checksums
and a GitHub Release.

**The CGO caveat — this drives the whole build matrix.** The daemon's
interactive terminal uses `github.com/creack/pty` (see
`internal/daemon/attach.go`), which relies on platform-specific PTY syscalls.
Treat the build as **not portably cross-compilable**: build each target on a
native runner for that OS rather than cross-compiling everything from one host.
In practice that means a GitHub Actions matrix with a `macos-latest` runner for
the Darwin archives and an `ubuntu-latest` runner for the Linux archives — not a
single `goreleaser release` invocation fanning out to every GOOS from one box.

Also remember the **`go:embed` ordering**: the web UI (`web/dist`) must be built
*before* GoReleaser compiles the Go binary, so `npm ci && npm run build` has to
run as a pre-build hook on every runner.

Sketch (`.goreleaser.yaml`):

```yaml
version: 2

before:
  hooks:
    # web/dist must exist before the Go build embeds it
    - sh -c "cd web && npm ci && npm run build"

builds:
  - id: warden
    main: ./cmd/warden
    binary: warden
    env:
      - CGO_ENABLED=0   # creack/pty is syscall-based; flip to 1 only if a
                        # target needs cgo. The point of the split runners is
                        # that each archive is produced on its native OS.
    ldflags:
      - -s -w
      - -X github.com/srjn45/warden/internal/buildinfo.Version={{.Version}}
      - -X github.com/srjn45/warden/internal/buildinfo.Commit={{.ShortCommit}}
      - -X github.com/srjn45/warden/internal/buildinfo.Date={{.Date}}
    goos: [darwin, linux]
    goarch: [amd64, arm64]

archives:
  - id: default
    formats: [tar.gz]
    files:
      - LICENSE
      - README.md
      - docs/SHIPPING.md
      - deploy/**         # ship the plist + systemd templates
      - skills/warden/**

checksum:
  name_template: "checksums.txt"

release:
  github:
    owner: srajanpathak
    name: warden
```

CI shape (`.github/workflows/release.yml`):

- Trigger on `push: tags: ['v*']`.
- **Two jobs**, one `runs-on: macos-latest`, one `runs-on: ubuntu-latest`, each
  running `goreleaser release --split` (or building only its own GOOS), then a
  final job to merge/publish (`goreleaser continue --merge`). This keeps each
  PTY-dependent binary built on hardware that matches its target.
- Set up Go (1.26+) and Node on each runner before the GoReleaser step.
- macOS job: optionally Developer-ID sign + notarize the binary for Gatekeeper
  (the current self-signed `codesign-setup.sh` identity is for *local* FDA
  persistence, not for distribution — distributed binaries want notarization or
  users will hit quarantine warnings).

### 4.4 systemd unit (Linux service)

**Goal:** a Linux parallel to the launchd plist — run the daemon as a managed
user service.

Ship `deploy/warden.service.template` and have a Linux installer path render
and load it as a **user** unit (`systemctl --user`), mirroring the per-user
launchd `LaunchAgent` model (no root, runs in the user's session):

```ini
[Unit]
Description=warden daemon
After=network.target

[Service]
ExecStart=__BINARY__ daemon
Restart=always
RestartSec=2
Environment=WARDEN_ADDR=__ADDR__
Environment=WARDEN_APPROVALS=on
Environment=WARDEN_SPAWN_GATE_MAX_AGENTS=10
Environment=TERM=xterm-256color
# inherit a sane PATH for tmux/git/claude/gh
Environment=PATH=__HOME__/.local/bin:/usr/local/bin:/usr/bin:/bin

[Install]
WantedBy=default.target
```

Notes:

- Use the **same `sed` placeholder substitution** (`__BINARY__`, `__ADDR__`,
  `__HOME__`) the plist template already uses, so `render_plist` and a new
  `render_unit` share logic in `common.sh`.
- Install to `~/.config/systemd/user/<label>.service`; load with
  `systemctl --user daemon-reload && systemctl --user enable --now <label>`.
- `Restart=always` is the systemd analogue of launchd `KeepAlive=true`;
  `WantedBy=default.target` + `loginctl enable-linger` gives the
  `RunAtLoad`/start-at-login behavior across logins.
- Set `TERM` explicitly (same reason the launchd daemon does): a service-managed
  process inherits no terminal, and tmux/PTY attach needs it.
- The install script should branch on `uname` (or a `--service-manager` flag):
  launchd on Darwin, systemd on Linux. The label, health probe, skill copy, and
  MCP registration steps are platform-independent and stay shared.

### 4.5 Homebrew tap (primary macOS channel)

**Goal:** the headline install for the macOS-first audience is
`brew install srajanpathak/tap/warden`.

GoReleaser can generate and push the formula automatically from the release
archives. Add a `brews:` block to `.goreleaser.yaml` pointing at a tap repo
(e.g. `srajanpathak/homebrew-tap`):

```yaml
brews:
  - name: warden
    repository:
      owner: srajanpathak
      name: homebrew-tap
    homepage: "https://github.com/srjn45/warden"
    description: "Spawn, monitor, and tear down coding-agent sessions"
    license: "MIT"   # set to the real license
    dependencies:
      - tmux
      - git
      - gh
    # claude is not in Homebrew core — call it out in the caveat instead.
    caveats: |
      warden needs the Claude Code CLI on your PATH:
        https://docs.claude.com/claude-code
      Start the daemon as a launchd service:
        warden service install      # (planned installer subcommand)
      Then run `warden doctor` to verify tmux/git/claude/gh and daemon health.
    service: |
      run [opt_bin/"warden", "daemon"]
      keep_alive true
      log_path var/"log/warden.log"
      error_log_path var/"log/warden.err"
```

Considerations:

- **Formula `service` block vs. our launchd installer.** Homebrew can manage the
  daemon via `brew services start warden` using its own generated plist. That
  is simpler for users but uses Homebrew's label and paths, *not*
  `com.srajanpathak.warden` + `~/.local/bin`. Decide one of:
  (a) lean on `brew services` and retire the bespoke plist for tap users, or
  (b) keep the formula binary-only and have an `warden service install`
  subcommand own the plist (recommended — keeps macOS and Linux service setup
  going through the same code path as §4.4).
- **`claude` dependency.** The Claude Code CLI isn't in Homebrew core, so it
  can't be a formula `depends_on`. Surface it in `caveats` and have
  `warden doctor` check it.
- **Skill + hook + MCP** are warden-specific and don't belong in a generic
  formula. Move them behind an `warden install` / `warden service install`
  subcommand the user runs once post-`brew install`, replacing the bash
  installer for tap users while the scripts remain for from-source installs.
- Tag the formula license to match the repo's actual `LICENSE`.

---

## 5. Suggested sequencing

1. **§4.1 version + doctor** — smallest change, immediately makes every later
   step debuggable in the field.
2. **§4.2 de-hardcode** — unblocks installing on a machine that isn't the
   author's; prerequisite for any channel.
3. **§4.3 GoReleaser** — produces the artifacts the next two steps consume.
4. **§4.4 systemd** + finish the cross-platform installer branch.
5. **§4.5 Homebrew tap** — the user-facing front door, built on top of the
   release artifacts.

Each step is independently shippable and leaves the current `make release` /
`make install` from-source flow working throughout.
