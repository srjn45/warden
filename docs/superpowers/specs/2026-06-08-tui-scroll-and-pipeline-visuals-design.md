# TUI: inspector scroll + pipeline/job status visuals

Date: 2026-06-08
Status: approved (design)

## Problem

Two issues in the cockpit's top-left list pane (`internal/tui`):

1. **Inspector not scrollable.** Pressing `c` opens the ctx/messages inspector
   (`modeInspector`), which renders `inspectorBody(...)` into a fixed-height box
   with no windowing. Long shared-context / message lists overflow and are
   clipped with no way to scroll. (The normal agent-list view *does* scroll via
   the cursor + `▲/▼ N more`; the master and detail panes scroll via tmux
   copy-mode. The inspector is the one place with no scroll path.)

2. **Pipeline & job statuses are visually flat.** `jobGlyph` returns monochrome
   glyphs and the status word is rendered muted-grey for every state, so
   pending/running/done/failed/etc. are hard to distinguish at a glance.
   Pipelines have no clear "partial failure" indication.

## Scope

`internal/tui/` only. No changes to the `pipeline` backend, store, daemon, web,
or CLI. Pure display logic + local Bubble Tea model state.

## Design

### 1. Inspector scroll (`list_pane.go`, `inspector.go`)

- Add a `github.com/charmbracelet/bubbles/viewport` field to `listPaneModel`,
  used **only** in `modeInspector`.
- Entering `c`: set viewport content to `inspectorBody(...)` and reset offset to
  the top.
- On each refresh tick while in inspector mode: re-`SetContent` but **preserve
  `YOffset`** so the view does not snap back to the top every second.
- On `WindowSizeMsg`: size the viewport to the pane body (width `w-4`, height
  `bodyH-2`, matching the existing `titleBox` interior).
- `modeInspector` key handling: `↑/↓` scroll (plus `pgup/pgdn`, `g/G` as free
  viewport bindings). `esc`/`c` exit, `q`/`ctrl+c` quit — unchanged.
- Normal-mode `↑/↓`/`j`/`k` (cursor movement) are untouched.
- No mouse capture is enabled (keyboard only) — zero interaction with tmux
  mouse handling. The footer hint gains a scroll note.

### 2. Colored status glyphs (`styles.go`, `list.go`, `pipeline_view.go`)

- Add a cyan style var (`stRunning`, color `6`); reuse existing green
  (`stBusy`), red (`stError`), amber (`stAttention`), grey (`stMuted`/`stIdle`).
- Replace `jobGlyph(s) string` with `jobBadge(s) (glyph string, style
  lipgloss.Style)`:

  | status            | glyph | color |
  |-------------------|-------|-------|
  | pending           | `○`   | grey  |
  | running           | `◐`   | cyan  |
  | done              | `●`   | green |
  | failed            | `✗`   | red   |
  | needs_attention   | `⚠`   | amber |
  | skipped           | `⊘`   | grey  |
  | partial *(derived, pipeline only)* | `◑` | amber |

- **Both the glyph and the status word** are rendered in the status color. The
  rest of the row (job id, deps, prompt/output) stays default so the line is not
  noisy.
- Apply in: job rows (`renderItemLine`), pipeline detail (`renderPipeline`,
  `renderPipelineJob`), and the pipeline header row.

### 3. Pipeline header status + collapse/expand (`list.go`, `list_pane.go`)

- New helper `pipelineDisplayStatus(p) (label string, style lipgloss.Style,
  glyph string)`. **Derived "partial":** if the pipeline status is terminal
  (`done` / `stalled` / `canceled`) **and** ≥1 job is `failed` or
  `needs_attention`, show `partial` (amber, `◑`). Otherwise map the real status:
  pending grey `○`, running cyan `◐`, done green `●`, stalled amber `⚠`,
  canceled grey `⊘`.
- Pipeline header row renders as `▾ <id>  ◐ running` — an expand indicator, the
  id (bold), then the colored status glyph + word.
- Add `collapsed map[string]bool` (keyed by pipeline id) to `listPaneModel`.
  `pipelineItems` takes it: a collapsed pipeline emits only its header row, no
  job rows.
- Expand indicator: `▾` = expanded, `▸` = collapsed.
- Keys (normal mode): `→`/`l` expand the pipeline under the cursor; `←`/`h`
  collapse it. If the cursor is on a *job* row, `←` collapses the parent
  pipeline and re-pins the cursor to the header (reusing the existing
  `itemKey`/`repin` machinery so the cursor never lands on a hidden row).
- Default = expanded (current behavior preserved). Help text and footer gain a
  `←/→ collapse/expand` hint.

## Testing

Pure functions get unit tests (project TDD convention):

- `jobBadge` — glyph + style for every `JobStatus`.
- `pipelineDisplayStatus` — each real status, plus the derived-`partial` cases
  (terminal pipeline with a failed / needs_attention job) and the negative case
  (terminal pipeline, all jobs done → not partial).
- `pipelineItems` — honors `collapsed` (header only when collapsed; header +
  jobs when expanded).
- Collapse cursor re-pin — collapsing while the cursor is on a hidden job moves
  the cursor to the pipeline header.

Viewport wiring (sizing, content refresh preserving offset, scroll keys) is
verified by manual TUI smoke, consistent with how existing pane interaction is
exercised.

## Non-goals

- No mouse-wheel scrolling in the list pane (avoids the known tmux/alt-screen
  mouse conflict).
- No real `StatusPartial` in the pipeline backend — "partial" is display-only.
- No changes to how the detail / master panes scroll (tmux copy-mode already
  handles them).
