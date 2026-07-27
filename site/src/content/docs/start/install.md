---
title: Install & setup
description: Prerequisites, installing the binary, running the daemon as a service (launchd on macOS, systemd on Linux), and wiring in the Claude Code hooks.
---

## Prerequisites

- **Go 1.26+** — to build the binary (only needed for `go install` or building from source)
- **tmux** — every agent session runs in a detached tmux window
- **git** — worktree creation and guarded cleanup
- **Claude Code** (`claude` on PATH) — the default agent runtime launched in each session (other backends are opt-in via `--backend` — see [Agent backends](/warden/concepts/agent-backends/))
- **`gh`** (GitHub CLI) — required for `pr-review` sessions to check out the PR branch
- **Ollama** (optional) — only needed if you enable the local-LLM features (`local_llm`) or the `warden repl` REPL; warden falls back to Claude when it's off or unreachable

:::tip
Once you have the `warden` binary, run **`warden doctor`** to check these
dependencies and **`warden setup`** to install whatever is missing (Homebrew on
macOS; apt/dnf/pacman on Linux; official installers for Claude Code and Ollama).
`warden setup --yes` does it non-interactively.
:::

## Install

warden is one self-contained binary. Pick whichever fits:

### 1. Download a release binary (quickest)

Grab the archive for your OS/arch from the [latest release](https://github.com/srjn45/warden/releases/latest), extract `warden`, and put it on your `PATH`. Released binaries have the web dashboard embedded.

```sh
# example: macOS arm64 (adjust the version/arch)
curl -fsSL https://github.com/srjn45/warden/releases/latest/download/warden_8.16.3_darwin_arm64.tar.gz | tar -xz
sudo mv warden /usr/local/bin/        # or any dir on your PATH
warden --version
```

> **macOS Gatekeeper:** downloaded binaries are unsigned, so the first run may be blocked. Clear the quarantine flag once: `xattr -d com.apple.quarantine $(which warden)` (or right-click → Open). Building from source — option 3 — avoids this.

### 1b. Package managers (Homebrew / deb / rpm / AUR)

Every release also ships native packages — all include the `wd` alias and the embedded web dashboard:

```sh
# macOS — Homebrew tap (clears the Gatekeeper quarantine for you)
brew install --cask srjn45/tap/warden

# Debian / Ubuntu
curl -fsSLO https://github.com/srjn45/warden/releases/latest/download/warden_8.16.3_linux_amd64.deb
sudo apt install ./warden_8.16.3_linux_amd64.deb

# Fedora / RHEL
sudo dnf install https://github.com/srjn45/warden/releases/latest/download/warden_8.16.3_linux_amd64.rpm

# Arch (AUR)
yay -S warden-bin
```

(Adjust the version/arch in the URLs. The `.deb`/`.rpm` pull in `tmux` and `git` as recommended packages; `warden setup` installs anything still missing.)

### 2. `go install` (Go toolchain)

```sh
go install github.com/srjn45/warden/cmd/warden@latest
```

This installs the `warden` binary (CLI + daemon + MCP server + TUI). **Note:** `go install` does *not* bundle the web dashboard (the UI is built from `web/` and embedded at release time, and isn't committed to the repo). The CLI, daemon API, TUI, and MCP server all work; for the embedded web GUI use a release binary (option 1) or build from source (option 3).

### 3. Build from source

```sh
git clone https://github.com/srjn45/warden.git
cd warden
make build           # CLI/daemon/TUI only → bin/warden
make release         # builds the web UI first, then embeds it → full GUI
```

## Install the daemon as a service (auto-start)

One installer covers both platforms — it **auto-detects your OS** and wires up the native service manager (launchd on macOS, systemd on Linux). It builds the release, installs the binary to `~/.local/bin/warden`, renders and loads the service unit, links the Claude skill, and registers the MCP server:

```sh
./scripts/install.sh        # or: make install
```

The daemon then starts automatically, restarts on crash, and listens on `127.0.0.1:8765` by default. The same script powers `./scripts/reinstall.sh` (redeploy after a code change) and `./scripts/uninstall.sh` (covered below) on both platforms.

> `~/.local/bin` must be on your `PATH` to run `warden` from the shell — the installer warns if it isn't.

### Linux (systemd user service)

On Linux the installer requires a **systemd user session** (`systemctl` on `PATH`) and installs warden as a **per-user** unit — no `sudo`, nothing system-wide. It renders `deploy/warden.service.template` to `~/.config/systemd/user/warden.service`, then:

```sh
loginctl enable-linger "$USER"        # keep the daemon alive after logout (≈ launchd RunAtLoad)
systemctl --user daemon-reload
systemctl --user enable --now warden  # start now + on every login
```

`enable-linger` is what lets the daemon survive between SSH/terminal sessions. The installer runs all of this for you; you only need the commands below to manage it afterward:

```sh
systemctl --user status warden     # is it running?
systemctl --user restart warden    # apply a rebuilt binary or config change
systemctl --user stop warden       # stop without disabling
journalctl --user -u warden -f     # follow the unit's own journal
```

The unit also writes stdout/stderr to the same log files used on macOS (see [Logs](#logs) below). When you enable [remote access](/warden/guides/remote-access/), the unit gains an `EnvironmentFile=` line pointing at your token file so the daemon picks up `WARDEN_TOKEN` on start; `warden token rotate` issues `systemctl --user restart warden` to apply a new token.

### Stop macOS "warden would like to access…" prompts (optional, macOS)

The launchd daemon is the macOS TCC *responsible process* for the agents it spawns and for its own directory picker, so reads of protected folders (Downloads, Documents, Desktop, the Music/media library) surface as *"warden would like to access…"* prompts. Granting Full Disk Access once silences them — but macOS ties the grant to the binary's code identity, and an unsigned Go binary gets a new identity on every rebuild, which brings the prompts back.

Run the one-time setup to give the binary a **stable** self-signed identity:

```sh
./scripts/codesign-setup.sh   # creates a self-signed code-signing cert (once)
./scripts/install.sh          # reinstall so the binary is signed
```

Then grant access once: **System Settings → Privacy & Security → Full Disk Access → "+"** and add `~/.local/bin/warden`. Because the signing identity is stable, the grant survives future rebuilds. (`install.sh`/`reinstall.sh` sign automatically when the cert exists; without it they warn and leave the binary unsigned.)

**Redeploy after a code change** (replaces `make release && ./bin/warden daemon`):

```sh
./scripts/reinstall.sh             # rebuild UI + binary, redeploy, restart
./scripts/reinstall.sh --no-build  # redeploy the existing build only
# or: make reinstall  /  make reinstall NO_BUILD=1
```

**Uninstall** (stops and removes the service, binary, skill link, and MCP registration; **preserves** your session store at `~/.warden` and the logs):

```sh
./scripts/uninstall.sh                 # or: make uninstall
./scripts/uninstall.sh --keep-binary   # leave ~/.local/bin/warden in place
```

### Logs

- **macOS (launchd):** files under `/tmp`
  - stdout: `/tmp/warden.daemon.log`
  - stderr: `/tmp/warden.daemon.err`
- **Linux (systemd):** the systemd journal — the unit sets `StandardOutput=journal` / `StandardError=journal` so logs survive reboots even where `/tmp` is a tmpfs (Fedora, Arch, …):
  - `journalctl --user -u warden -f` (follow live)
  - `journalctl --user -u warden -n 100` (last 100 lines)

> **Notifications:** off by default. When enabled with `WARDEN_NOTIFY=on`, the daemon posts a macOS notification when an agent enters `waiting_for_input`, `idle` (stuck), `orphaned`, or `errored`. These appear only when the daemon runs in your GUI login session (a terminal, or a launchd **user agent**); a headless/system daemon logs them instead.

## Wire in the Claude Code hooks

The hook script posts lifecycle events (`SessionStart`, `Notification`, `Stop`, `SubagentStop`, `SessionEnd`) to the daemon so it can update agent status in real time without polling. `SessionEnd` marks the session **done** (terminal) when claude exits.

Merge `hooks/settings.snippet.json` into `~/.claude/settings.json`. The snippet uses a `__WARDEN_HOOK__` placeholder — substitute the absolute path to `hooks/warden-hook.sh` in your clone first:

```sh
# from the repo root, render the snippet with the real hook path:
sed "s|__WARDEN_HOOK__|$(pwd)/hooks/warden-hook.sh|g" hooks/settings.snippet.json

# If ~/.claude/settings.json doesn't exist yet, write it directly:
sed "s|__WARDEN_HOOK__|$(pwd)/hooks/warden-hook.sh|g" hooks/settings.snippet.json > ~/.claude/settings.json

# If it already exists, merge the rendered "hooks" key into the root of your
# existing settings.json object.
```

The hook fails soft — it never blocks or errors the agent, even if the daemon is down or the session is unknown.
