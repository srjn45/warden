---
title: Android connectivity status
description: How an eventual Android client connects to the shipped node API, and what is still planned.
---

There is no shipped Warden Android application. The Android design is a planning
document, so there is no APK, pairing flow, or Hub-based mobile connection to
configure today.

The design deliberately uses the existing [node API](/warden/guides/node-api-client-contract/)
rather than introducing a phone-specific server. That means any future Android
client can connect to a daemon you have already configured for direct remote
access.

## Current way to use a phone

The mobile-responsive web dashboard is available now. Configure
[remote access](/warden/guides/remote-access/) with a bearer token, then reach
the dashboard through a private overlay or authenticated tunnel. Do not expose
the daemon port directly to the public internet.

## Planned Android client connection model

The design calls for a user to enter a daemon base URL and bearer token (with QR
pairing only as a possible convenience). It would use:

- REST for fleet state and actions;
- SSE for live fleet updates;
- WebSockets for agent terminal attach and the cockpit terminal; and
- the same bearer-token rules as every other direct API client.

A read-only token can view GET/SSE data but cannot perform state-changing
actions or open an interactive attach session.

Hub-routed Android connectivity is also planned, not shipped. When available,
it is intended to use the same node API over a relay, with native end-to-end
authentication; it will not require a parallel Android-only backend.
