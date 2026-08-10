---
title: Re-authenticate a backend from your phone
description: When an agent's backend login expires while you're away from the machine, re-auth over a warden terminal using the paste-code flow — no browser on the host required.
---

Backend logins expire. When a running fleet suddenly starts failing on auth — a
Claude subscription token lapses, an OAuth session ages out — you often aren't
sitting at the machine. The instinct is "I need a browser on that box." For
Claude Code you don't: its login is a **paste-code OAuth flow**, so you can
complete it from any device with a browser and paste the result back over a
warden terminal.

## Why no browser on the host is needed

Run `claude auth login` and it prints a URL, then **waits at the terminal**:

```
Browser didn't open? Use the url below to sign in (c to copy)
https://claude.com/cai/oauth/authorize?…&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&code=true&…
```

Two details in that URL are the whole story:

- `redirect_uri=…platform.claude.com/oauth/code/callback` — the redirect lands on
  a **hosted Anthropic page**, not `http://localhost:<port>`. There is no local
  callback server, so nothing has to run a browser on the dev host.
- `code=true` — tells the authorize server to **display an authorization code**
  on that callback page for you to copy, rather than auto-posting it back to a
  loopback listener.

So the flow is: open the URL anywhere → authorize → copy the code the page shows
→ paste it into the terminal that's waiting. The machine running the agent never
needs a browser or a graphical session.

## What you need

- **Remote access to the daemon** — bind it off-loopback with a bearer token.
  See [Remote access & authentication](/warden/guides/remote-access/).
- **A way to reach a terminal on the host from your phone.** Any of:
  - a **terminal session** in the [web mission control](/warden/guides/web-mission-control/) dashboard,
  - **attach** to a running agent from the [TUI cockpit](/warden/guides/tui-cockpit/) or the mobile app's raw terminal,
  - a plain SSH/Tailscale shell — the paste-code flow doesn't care how you got the prompt.

A dedicated **terminal session** (`kind=terminal`) is the cleanest place to run
this: it re-auths the shared backend credentials without disturbing a running
agent's conversation.

## Steps

1. From your phone, open a terminal on the host (a warden terminal session, or
   attach to an agent).
2. Run the login command:
   ```sh
   claude auth login          # Claude subscription (default)
   # or, for a long-lived headless token:
   claude setup-token
   ```
3. It prints the sign-in URL. **Open that URL in your phone's browser.** (In the
   web dashboard's terminal the URL is usually tappable; from a raw terminal,
   long-press to copy it.)
4. Sign in and authorize. The hosted callback page then **displays an
   authorization code**.
5. **Copy the code and paste it back into the terminal** where the command is
   waiting, and press Enter.

Verify it took:

```sh
claude auth status
# { "loggedIn": true, "authMethod": "claude.ai", "subscriptionType": "max", … }
```

That's it — the fleet picks the refreshed credentials back up.

## `auth login` vs. `setup-token`

Both use the same paste-code mechanics; they differ in what they leave behind:

- **`claude auth login`** refreshes the normal interactive session — use it to
  clear an expired login in place.
- **`claude setup-token`** mints a **long-lived token** (requires a Claude
  subscription), which is the better fit for a headless or remote host you don't
  want to re-auth often.

## When this shortcut does *not* apply

This works for any backend whose login is a device-code or paste-code flow
(Claude Code is one). A backend that **strictly requires a loopback-redirect
browser** on the host can't be completed by paste-code alone — those would need
an actual browser where the redirect lands. That gap is what a future
first-class browser session would close; for Claude Code today it simply doesn't
arise.
