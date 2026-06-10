# TUI agent-detail toggle + approvals key rebind — design

**Date:** 2026-06-10
**Scope:** TUI only (`internal/tui`). No daemon, client, API, web, or CLI changes.

## Problem

The cockpit agent-list row (`renderItemLine`, `internal/tui/list.go:507-513`) packs seven
columns into fixed widths and gives the subject `width − 51` characters. On a narrow
cockpit list pane the subject clips to nothing, and most agent data (dir, branch,
worktree, mode flags, ticket/PR, pipeline membership, timestamps, prompt) is never
shown in the list at all — only reachable by attaching.

Separately, the approvals overlay is bound to `i`, which is a poor mnemonic, and pressing
it appears to "do nothing": the handler only switches to the overlay when the approvals
queue is already non-empty, and gives no feedback otherwise.

## Goals

1. The always-visible row shows only what's needed to scan, in fixed columns that never
   clip: **ID, status, context, age**.
2. All other agent data is reachable via a same-pane toggle for the selected agent —
   mirroring how `c` (inspector), `d` (digest), and the approvals overlay already replace
   the list body inside the same framed pane.
3. Rebind approvals off `i` to a sensible key, and give feedback when the approvals key is
   pressed with an empty/disabled queue.

## Decisions (settled with user)

- Row keeps **ID · status · context · age** only. `Type` and `Subject` move into the detail view.
- New agent-detail view is toggled by **`i`** ("info"), rendered in the same framed pane.
- Approvals moves from `i` to **`p`** ("permission prompts").
- Pressing `p` with an empty queue shows a **footer status message** (not an empty overlay,
  not silent): `"approvals disabled (set WARDEN_APPROVALS)"` when `m.apprEnabled` is false,
  `"no approvals pending"` when enabled but empty.

## Architecture

The list pane is a single bubble-tea model (`listPaneModel`) with a `mode` field. Overlay
modes (`modeInspector`, `modeDigest`, `modeApprovals`) each replace `renderList(...)` inside
the framed `titleBox` in `View()` (`internal/tui/list_pane.go:663-674`) and toggle back to
`modeNormal`. This change adds one more such mode and follows the existing pattern exactly.

### Change 1 — lean the row

In `renderItemLine`'s `default` case (`list.go:507-513`), reduce the format string to the
four fixed columns. ID(12) · status(11) · context(6) · age(5). Drop `Type` and `Subject`
and the `width-51` subject slice. The row width becomes a fixed ~36 chars and never clips
regardless of pane width. The cursor caret, group-dir header, approvals row, and
pipeline/job rows are unchanged.

### Change 2 — `detailBody(s *store.Session) string`

New pure render function in `list.go` (alongside `digestBody`/`approvalsBody`). Takes a
`*store.Session` and returns a multi-line, grouped summary. All data comes from fields
already on `store.Session` — no fetch, no daemon call. Empty fields are omitted so the
view stays tight. Sections (omit a whole section if all its fields are empty):

```
<id>                                    <status> · <supervised?>

  subject   <Subject>
  type      <Type>            age <age>   created <ago>
  context   <Nk> (<state>) · checked <ago> · last /compact <ago>

location
  dir       <Repo or Workdir>
  branch    <Branch>
  worktree  <Worktree>

refs
  ticket    <Ticket>          pr  <PR>
  pipeline  <PipelineID>      job <JobID>

mode
  <supervised|bypass> · auto-restart ×<RestartCount> (last <ago>)

plumbing
  pid <PID> · tmux <TmuxSession> · claude <ClaudeSessionID short>
  prompt    "<Prompt truncated>"
```

Exact labels/spacing are an implementation detail; the grouping above is the contract.
Reuses existing helpers (`age`, `contextLabel`, `abbrevHome`, `trunc`).

### Change 3 — `modeDetails` wiring in `list_pane.go`

- Add `modeDetails` to the mode enum and a `detailID string` field (parallel to `digestID`),
  or render directly from the selected session each frame. Render uses the scrollable
  viewport `m.vp` exactly like digest: on open, `m.vp.SetContent(detailBody(s))` +
  `m.vp.GotoTop()`.
- **Normal-mode `i` handler:** if `m.selected() != nil`, set `m.mode = modeDetails`,
  populate the viewport, and `GotoTop()`. Otherwise no-op (cursor on a header/approvals row).
- **`modeDetails` key handling:** add a case to the mode switch (copy the `modeDigest`
  block at `list_pane.go:478-488`): `q`/`ctrl+c` quit; `i`/`esc` back to `modeNormal`;
  `g`/`G` and default → forward to `m.vp.Update` for scrolling.
- **`View()`:** add a `modeDetails` branch (copy the `modeDigest` branch at line 667-669):
  `titleBox("Details — "+id, m.vp.View(), m.w, bodyH)` with footer
  `"↑/↓ pgup/pgdn g/G scroll · i/esc back · q quit"`.

### Change 4 — approvals rebind to `p` + empty feedback

- Move the normal-mode `case "i"` approvals block (`list_pane.go:630-634`) to `case "p"`.
  When `recognizedApprovals(m.approvals)` is empty, instead of no-op set `m.status`:
  `"approvals disabled (set WARDEN_APPROVALS)"` if `!m.apprEnabled`, else `"no approvals pending"`.
- In the `modeApprovals` handler, change the toggle-back key from `"esc", "i"` to
  `"esc", "p"` (`list_pane.go:498`).
- Update help text (`internal/tui/view.go`) and any footer teaser that references the keys:
  approvals `i`→`p`, add `i` = agent details.

## Testing

`internal/tui` has table/golden-style tests. Add unit tests:

- `detailBody`: full session renders all sections; session with empty
  ticket/PR/worktree/pipeline omits those sections; unknown context renders no context
  figure; long prompt is truncated.
- Lean row: `renderItemLine` for an agent produces only the four columns and does not clip
  at small widths (e.g. width 30 still shows status/context/age intact).
- Key handling (if the existing harness drives `Update`): `i` on a selected agent enters
  `modeDetails`; `i`/`esc` exits; `p` with empty disabled queue sets the disabled status
  string; `p` with empty enabled queue sets the "no approvals pending" status.

## Out of scope (YAGNI)

- No new data sources (RSS/CPU metrics, digest, live pane excerpt) in the detail view —
  those have their own surfaces (`stats`, `d`). v1 renders only `store.Session` fields.
- No inline tree-expand of rows; the detail is a full-pane overlay like the others.
- No web/CLI parity for the new view.
- No change to the approvals feature gating itself (`WARDEN_APPROVALS` stays off by default).
