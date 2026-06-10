# Warden Mobile App — Design Spec

**Date:** 2026-06-10  
**Status:** Future enhancement — high-level design. Detail pass required before implementation. Phase 2 after remote-access (Phase 1) ships.

---

## Summary

A cross-platform native mobile app (iOS + Android) for warden. Phase 1 (mobile-responsive web UI + auth) is covered in the Remote Access spec — this spec covers the Phase 2 native app.

**Prerequisite:** Remote access spec must be shipped first (bearer token auth, `WARDEN_BIND_ADDR`, Tailscale/Tunnel docs). The native app uses the same daemon API.

---

## Tech Stack

**React Native + Expo** — cross-platform iOS + Android from a single TypeScript codebase.

- Expo managed workflow (EAS Build for distribution)
- TypeScript throughout
- Reuse API types from the existing `web/src/lib/` (extract to a shared package or copy)
- No Electron, no Flutter — React Native gives maximum code sharing with the existing React web UI logic

---

## Core Screens

| Screen | Description |
|---|---|
| **Connect** | Enter daemon URL + token. QR code scan option (scan the token from the web UI). Saved in SecureStore. |
| **Agent List** | Scrollable list of agents with status badge, model, context size, age. Pull-to-refresh. |
| **Agent Detail** | Status, events timeline, context gauge, digest button, terminate button. |
| **Terminal** | Full-screen interactive terminal via the existing WS `/sessions/{id}/attach` endpoint. xterm.js in a WebView, or a native terminal package (evaluate at implementation time). |
| **Approvals** | Pending permission prompts with 1-tap answer buttons. Push notification on new approval. |
| **Metrics** | Summary cards (system memory, agent count, pressure level). Sparklines for top agents. Full graphs deferred to tablet layout. |
| **Pipelines** | List of active/archived pipelines. Job status grid. Tap to see job detail / attach terminal. |

---

## Network

Same as Phase 1 (Remote Access spec):
- **LAN:** `WARDEN_BIND_ADDR=0.0.0.0:7979` + phone on same WiFi
- **WAN:** Tailscale (recommended) or Cloudflare Tunnel

The app connects directly to the daemon — no relay server, no cloud backend.

---

## Auth

Bearer token stored in **Expo SecureStore** (encrypted on-device, backed by Keychain on iOS, Keystore on Android). Sent as `Authorization: Bearer <token>` on all API calls. Token passed as `?token=<t>` on WS/SSE connections (same as web).

**QR code pairing:** Web UI can display a QR code encoding `warden://<host>:<port>?token=<token>`. App scans it to configure the connection in one step — eliminates manual token entry on mobile.

---

## Push Notifications

| Event | Notification |
|---|---|
| Agent waiting for approval | "Agent X needs your approval" → tap opens Approvals screen |
| Agent done | "Agent X finished" → tap opens Agent Detail |
| Agent error / crash | "Agent X encountered an error" |
| Critical memory pressure | "Warden: memory critical" |

Delivery via **Expo Push Notifications** (APNs on iOS, FCM on Android). The daemon sends a push request to Expo's push API when an event warrants notification. This requires:
- A small cloud-side relay or the user's own Expo push token sent from app → daemon on connect
- The daemon stores the push token in memory and calls the Expo push API on matching events

**Alternative with no cloud dependency:** Local notifications only (app polls daemon in background, triggers local notification). Less reliable but zero server infrastructure.

---

## Tablet / iPad Layout

On larger screens (≥768pt): two-column layout — agent list on left, detail/terminal on right. Metrics shows full uPlot charts (WebView with the existing web UI metrics components, or native re-implementation). Matches the web UI's desktop layout intent.

---

## What the App Does NOT Do

- No spawning agents (Phase 2 scope — read/monitor/approve only)
- No pipeline creation
- No file system access (no DirPicker)
- No code editing

These restrictions keep the first native app focused and reviewable. Spawning from mobile is a Phase 3 consideration.

---

## API Compatibility

The daemon API does not change for this app — it uses the same endpoints as the web UI. One addition:

- `POST /push-token` — app registers its Expo push token with the daemon on connect. Daemon stores it in memory (single active token per daemon instance). Token cleared on app disconnect or explicit logout.

---

## Development / Distribution

- Development: Expo Go app for local testing without a build
- Distribution: EAS Build → TestFlight (iOS) + Google Play internal track
- No App Store submission required for personal use — sideload via TestFlight / direct APK

---

## Open Questions for Detail Pass

1. Terminal on mobile: xterm.js in a WebView (reuses existing web terminal exactly) vs. a native terminal package (`react-native-terminal`)? WebView is simpler; native feels better.
2. Background refresh on iOS is heavily restricted — does the approval notification model need a persistent WS connection (battery drain) or polling?
3. QR code pairing: implement in Phase 2 or defer? High UX value, low complexity.
4. Tablet layout: native re-implementation of charts, or just embed the existing web UI in a WebView for the metrics screen?
