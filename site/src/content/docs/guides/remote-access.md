---
title: Remote access & authentication
description: Reach the dashboard and API from another device with bearer-token auth, and deploy the daemon in a container.
---

By default the daemon binds `127.0.0.1:8765` — loopback only. To reach the dashboard and API from a phone, tablet, or another machine, bind a non-loopback address and gate it with a bearer token.

## Bearer-token auth

A 256-bit token gates **every** non-loopback request (constant-time compare); loopback requests stay unauthenticated. Binding the daemon to a non-loopback address is **refused unless a token is set**, so remote access can't be opened by accident. Auth failures are per-IP rate-limited.

```sh
warden token generate   # mint a token and persist it
warden token show       # print the current token (paste into a remote client)
warden token rotate     # regenerate in place and restart the daemon
```

The token is stored in `~/.warden/token.env` (`WARDEN_TOKEN=<hex>`, mode `0600`). The `WARDEN_TOKEN` environment variable overrides the file, so the secret can stay off disk in a container or CI.

To actually listen off-loopback, set a token (above) and bind the address you want (`addr` / `WARDEN_ADDR`). A bearer token is **mandatory** for any non-loopback bind — the daemon refuses to start on a non-loopback address without `WARDEN_TOKEN`. (The old `allow_nonloopback` flag is deprecated and inert; it no longer disables auth.)

- **CLI/API clients** send the token as a bearer header; SSE and WebSocket clients pass it as `?token=`.
- **The web UI** shows a token-entry modal on a `401`, persists it in `localStorage`, and offers a sign-out control. The static SPA shell stays public so the modal can load.
- **Mobile-responsive dashboard** — bottom nav, single-column grids, and full-screen modal sheets make the GUI usable on a phone.

> Prefer a private overlay (Tailscale) or an authenticated tunnel (Cloudflare Tunnel) over exposing the port directly to the internet. Step-by-step LAN / Tailscale / Cloudflare recipes live in the repo's `docs/USAGE.md`.

## Read-only access

To hand out view-only access — a wall dashboard, or a teammate who should watch but not act — set an optional **second** token, `WARDEN_READONLY_TOKEN`. A request bearing it may read everything (every GET plus the live event stream) but is denied all state-changing actions and the interactive attach (which can type into a running agent); those return HTTP `403`.

```sh
export WARDEN_READONLY_TOKEN=$(warden token generate)   # mint one (a token is just a random secret)
warden token show --readonly                             # print it back to paste into a viewer
```

The token value is identical to a normal one — what makes it read-only is the env var you assign it to. You can also add a `WARDEN_READONLY_TOKEN=<hex>` line to `~/.warden/token.env`. It is only honored **alongside** a primary `WARDEN_TOKEN`: the daemon refuses to start with a read-only token but no primary one (otherwise auth would be off entirely and "read-only" would silently grant full access). Revoke by regenerating it and restarting, exactly like the primary token.

> Need per-teammate tokens with individual expiry or revocation? That's a future escalation (a token registry); today there is one full token and one optional read-only token.

## Container deployment

A multi-stage `Dockerfile` and `docker-compose.yml` package the daemon for containerized remote access; the auth model above carries over unchanged.

```sh
WARDEN_TOKEN=$(openssl rand -hex 32) docker compose up -d
```

- **Lean image** — the web dashboard is built and `go:embed`-ed into a static `CGO_ENABLED=0` binary; the runtime stage is `alpine` carrying only the binary plus `tmux`, `git`, and `ca-certificates`, running as an unprivileged user.
- **Persistent state** — `~/.warden` (the session store + config) is a named volume, so records survive restarts. Imported records remember absent worktrees rather than recreating them.
- **Remote-access defaults** — the entrypoint binds `0.0.0.0:8765`; compose maps the port and threads `WARDEN_TOKEN` from the host (required — the daemon refuses a non-loopback bind without it).
- **tmux/claude boundary** — the image ships `tmux` + `git` and runs the daemon/API/dashboard out of the box. It deliberately omits the `claude` CLI to stay lean; driving live agents additionally needs `claude` + credentials layered in.
