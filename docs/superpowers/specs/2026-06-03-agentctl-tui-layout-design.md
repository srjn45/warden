# agentctl TUI Layout — Framed Panes + Scrollable List — Design

**Date:** 2026-06-03
**Status:** Approved design (pre-implementation)
**Owner:** Srajan Pathak (personal project)

---

## 1. Goal

Fix the TUI's two visible problems:
1. **Top list items get cut off.** `renderList` emits one line per session with no height limit or scroll window, so when the list (plus detail) is taller than the terminal, `View()` overflows and the terminal scrolls — pushing the header and top rows off the top.
2. **No proper frame distribution.** The list and detail panes are bare width-clamped strings with no frames and no shared height; the list isn't co-sized with the detail viewport.

Fix: clamp the list to the body height with a stateless scroll window that always keeps the selected row visible, and render both panes as equal-height rounded-border boxes with titles.

## 2. Current state (for reference)

- `view.go::View()` builds: `header` line, then `body = JoinHorizontal(Top, left, " ", right)` where `left=renderList`, `right=renderDetail` (each only width-constrained), then `footer`.
- `view.go::layout()` sizes `m.vp` (detail viewport) to `bodyH-6`, `m.ta`, `m.ti`.
- `list.go::renderList(width)` loops ALL `m.sessions` → one line each (no height clamp, no window).
- `detail.go::renderDetail(width)` joins: head, dir, subject, "", output-title, `m.vp.View()`, "", history.
- lipgloss `v1.1.0` (no native border-title → splice manually). No `Model` struct change needed.

## 3. Layout (view.go + layout())

Three vertical zones, total height = `m.h`:
- **Header** (1 line): `agentctl  live ●` (+ reconnecting/daemon-down text as today).
- **Body** (`bodyH = m.h - 2`, min 3): two rounded-border boxes side-by-side, **both exactly `bodyH` tall**.
- **Footer** (1 line): the status / hints line, or the mode prompt (new/send/confirm) as today.

Width split (account for each box's 2-col border so the row fits `m.w` with no wrap):
- `listOuter = m.w * 4 / 10` (min ~24; if `m.w` too small, clamp).
- `detailOuter = m.w - listOuter` (no separate gap — the borders provide separation; if a 1-col gap is used, subtract it).
- Inner content width of a box = `outer - 2`.

`layout()` recomputes on every `WindowSizeMsg`:
- `bodyH`, `listOuter`, `detailOuter`.
- `m.vp.Width = detailInner` ; `m.vp.Height = detailInner-height-for-viewport` (see §5).
- `m.ta`/`m.ti` unchanged.

## 4. Framed boxes (new `boxes.go`)

`titleBox(title, body string, outerW, outerH int) string`:
- Render `body` inside `lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Width(outerW-2).Height(outerH-2)` (border adds the 2 back → `outerW × outerH`).
- Splice `" "+title+" "` into the **top border line** of the result (overwrite columns 2…2+len in the first line, preserving the corner `╭` and trailing `─…╮`). A small string helper; truncate the title if wider than the inner width.
- Used for both panes (and optionally help).

## 5. List scroll window — the bug fix (list.go)

Add a **pure** helper (unit-testable, no `Model`):
```
listWindow(n, cursor, visible int) (top int)
// top = 0 while cursor < visible; once cursor passes the fold, top = cursor-visible+1; clamped to [0, max(0, n-visible)].
```
`renderList(width, height)`:
- Rows hidden? `hidden := len(sessions) > height`. When `hidden`, reserve the **last** row for a single combined hint line, so `visible = height - 1`; otherwise `visible = height`.
- `top := listWindow(len(sessions), m.cursor, visible)`.
- Render rows `[top, top+visible)` only (existing per-row formatting: id/type/badge/age/subject, cursor `›`).
- When `hidden`, append ONE muted hint line at the bottom showing only the relevant arrows: `▲ {top} more` if `top > 0`, and/or `▼ {n-top-visible} more` if `top+visible < n` (e.g. `▲ 2 more   ▼ 3 more`).
- Pad with blank lines so the rendered block is **exactly `height` lines** (box inner height).
- Empty list → the existing "No agents — press n to create one", padded to `height`.

This guarantees the list's rendered height == the box inner height, so the body can never exceed `bodyH` → no terminal overflow → header/top rows always visible.

## 6. Detail pane (detail.go)

- `renderDetail` is unchanged in content (head, dir, subject, blank, output-title, `m.vp.View()`, blank, history) — but the **viewport height** is set by `layout()` to `detailInner - fixedLines`, where `fixedLines` = head(1)+dir(1)+subject(1)+blank(1)+output-title(1)+blank(1)+history(≤7) ≈ 13. Clamp `m.vp.Height ≥ 1`. So the detail content fills its box without overflow.
- `renderDetail` output is placed into `titleBox(selectedID-or-"—", …, detailOuter, bodyH)`.

## 7. Modes & help

- `modeNewAgent`/`modeSendMsg`/`modeConfirmKill` render in the **footer area** as today (the boxes stay; the footer line becomes the prompt). Unchanged behavior.
- `modeHelp` stays a simple full-width block (optionally wrapped in `titleBox("Keys", …)`, but not required). The reducer/keys are unchanged.

## 8. Edge cases

- Tiny terminal: `bodyH`, inner widths/heights clamp to ≥ 1; boxes shrink; no stacking/responsive reflow (YAGNI).
- `m.cursor` out of range is already clamped by `repin`; `listWindow` also clamps.

## 9. Testing

- **`listWindow` (pure):** cursor at 0 → top 0; cursor at `n-1` with `n>visible` → top `n-visible`; cursor mid → window contains cursor; `n<=visible` → top 0. Table test.
- **`titleBox`:** the title text appears in the first (top-border) line; output is exactly `outerH` lines and `outerW` wide; over-long title is truncated; corners preserved.
- **`renderList(width,height)`:** output line count == `height`; with `n>height` the cursor row is present and a `▼`/`▲` hint line appears; selected row carries the cursor marker.
- **`View()`:** existing smoke test still passes (non-empty, contains header); add an assertion that with many sessions + a small height the output line count ≤ `m.h` (no overflow).

## 10. Files

- `internal/tui/view.go` — zone assembly, width/height math, `layout()`.
- `internal/tui/list.go` — `renderList(width, height)` + `listWindow` + hints.
- `internal/tui/detail.go` — minor (viewport sizing driven by `layout()`; no content change).
- `internal/tui/boxes.go` (new) — `titleBox` + the top-border title splice helper.
- Tests: `internal/tui/list_test.go` (extend), `internal/tui/boxes_test.go` (new), `internal/tui/model_test.go` (overflow smoke assertion).

## 11. Out of scope

- Responsive stacking on narrow terminals.
- Mouse support / resizable split / configurable ratio.
- Any change to the reducer, keybindings, polling, or data flow.
