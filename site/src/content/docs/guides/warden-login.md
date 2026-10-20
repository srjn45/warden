---
title: Sign a node in to Warden Hub
description: Use the shipped warden login device flow to provision a local node identity for a compatible Hub.
---

`warden login` is the node-side device-authorization command for a compatible
Warden Hub. It is intentionally separate from the daemon's existing bearer-token
remote access: signing in creates a certificate-based node identity; it does not
open a public listener or replace [direct remote access](/warden/guides/remote-access/).

> The command and its wire contract ship in warden. A generally deployable,
> hosted Warden Hub and its end-to-end relay connector are still planned. Use this
> flow only with a Hub that implements the compatible device endpoints.

## Start the device flow

```sh
warden login
```

By default the command targets `http://localhost:9876`. Point it at another
compatible Hub with `--hub`, or change the display name sent for this node with
`--hostname`:

```sh
warden login --hub https://hub.example.test --hostname build-host
```

The command prints a verification URL and short user code. Open that URL in a
browser, enter the code, and approve the request. Leave the terminal open while
it waits and polls for approval.

## What the node stores

Before polling, warden generates an ECDSA key pair locally and sends the Hub only
a PKCS#10 certificate-signing request. On approval, it stores the signed node
certificate, CA certificate, Hub URL, node ID, and the locally generated private
key under `~/.warden/identity`.

The private key never leaves the machine. Warden rejects an approval response
that includes a private-key field instead of silently accepting it.

## Expected outcomes

- While approval is pending, warden polls at the interval supplied by the Hub.
- If the Hub asks it to slow down, warden increases the polling interval.
- If the code expires, run `warden login` again.
- If approval is denied, the command stops without storing credentials.

This flow provisions identity only. Today it does not by itself start a daemon
relay connection, list remote nodes, or make the local dashboard reachable from
the internet; those Hub-dependent capabilities remain planned.
