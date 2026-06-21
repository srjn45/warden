# Auto-Approve Hardening — Design

**Date:** 2026-06-21
**Status:** Proposed (brainstorm), pending implementation plan

## Problem

The auto-approve feature (2026-06-16-auto-approve-design.md, now implemented) lets
the daemon answer recognized yes/no permission prompts on behalf of an agent so
unattended pipelines and overnight workflows don't stall. As shipped it is a blunt
instrument: when enabled it **blindly presses `1`** on every prompt
`approval.Parse()` recognizes, with no notion of *which* prompt, *which tool*, or
*what the option actually does*. That makes it unsafe to leave on:

1. **Hardcoded option 1.** The worker always sends `1`, assuming option 1 is the
   benign "yes". Claude Code prompts increasingly render a sticky variant as option
   1 ("Yes, and don't ask again for `Bash` commands in this project"), with the
   one-shot "Yes" lower in the list. Pressing `1` then grants a *standing*
   permission the user never intended — the opposite of least privilege.

2. **All-or-nothing.** `Session.AutoApprove` is a single boolean. Turning it on
   approves *everything* recognized — `Read` and `rm -rf` are treated identically.
   There is no way to say "auto-approve reads and edits, but always ask me about
   shell commands."

3. **No destructive guard.** A recognized prompt for a destructive, irreversible
   action (delete, force-push, publish, overwrite) is auto-confirmed exactly like a
   read. A single misconfiguration — or a sticky option-1 grant from weakness #1 —
   can let an agent quietly `git push --force` or `rm -rf` unattended, with no
   backstop.

This spec hardens auto-approve along all three axes and proposes a **staged
rollout**: a small Stage A that makes the *current* boolean safe by default, and a
larger Stage B that makes auto-approve genuinely *selective* via an allow/deny
policy modeled on Claude Code's own permission allow-list.

## Goals

- Never grant a *standing* (sticky) permission unless the user explicitly opts in;
  default to the least-privilege "yes, once" affirmative.
- Let users scope auto-approval to specific tools / prompt patterns / paths rather
  than all-or-nothing.
- Guarantee that a recognized destructive action can never be auto-confirmed by
  configuration alone — a built-in deny-list wins over any allow.
- Land the safety-critical pieces (affirmative selection + destructive guard)
  first, with no config-schema change, so the existing boolean becomes safe
  immediately.
- Reuse the existing `approval.Parse` / `SendKeys` / poller-worker plumbing.

## Non-Goals

- Auto-approving multi-select prompts, text-entry fields, or unrecognized/freeform
  prompts (unchanged — still skipped and left for a human).
- Natural-language understanding of arbitrary option text. Affirmative detection
  and destructive detection are deterministic keyword/heuristic matchers, not a
  model call.
- Per-option model classification or an LLM-judge in the approval path (latency and
  trust cost too high for the poller loop).
- Importing Claude Code's actual `settings.json` permission rules. We model the
  *shape* of an allow/deny list; we do not parse Claude's own config.

## Current Behavior (as implemented)

- **`internal/poller/poller.go:169` `tryAutoApprove()`** — gate is
  `if !p.AutoApproveGlobal && !s.AutoApprove { return }`
  (`poller.go:171`). It then calls `approval.Parse(pane)` (`poller.go:176`) and, on
  any recognized prompt with ≥1 option, unconditionally sends the literal key `"1"`
  via `p.deps.SendKeys(ctx, s.TmuxSession, "1")` (`poller.go:183`). The selected
  option is never inspected — `a.Options[0]` is only read for the success log line
  (`poller.go:188`). There is no tool/pattern classification and no destructive
  check anywhere in the path.
- **`internal/poller/poller.go:196` `runApprovalWorker()`** — drains
  `ApprovalEvents` and calls `tryAutoApprove` per event; events are published on
  transition-to / pane-change-while `waiting_for_input` (`poller.go:267`,
  `poller.go:288`).
- **`internal/approval/approval.go:33` `Parse()`** — returns an `Approval`
  (`approval.go:14`) with `Action`, `Question`, `Options []string` (1-indexed by
  position), and `SelectedIdx` (the `❯`-highlighted default). It already extracts
  everything we need to classify a prompt; it just doesn't tell the caller which
  option is the *affirmative* one or whether that option is *sticky*.
- **`internal/store/types.go:113` `Session.AutoApprove bool`** — the single
  all-or-nothing toggle; comment even reads `// always option 1`.
- **Config** — `auto_approve: bool` is a flat key in `~/.warden/config.yaml`
  (2026-06-17-config-file-design.md). `AutoApproveGlobal` is wired from it.

## Proposed Design

### Weakness 1 — Affirmative-option selection (replaces hardcoded `1`)

Move the decision of *what to press* out of the poller and into `approval`, where
the option labels already live.

Extend the parsed result so a caller can answer correctly:

```go
// internal/approval/approval.go
type Approval struct {
    Action      string
    Question    string
    Options     []string
    SelectedIdx int

    // Affirmative classification (new).
    AffirmativeIdx int  // 1-based index of the least-privilege "yes" option; 0 if none found
    AffirmativeSticky bool // true if the ONLY affirmative is a "don't ask again"/sticky grant
}
```

Add `classifyOptions(opts []string)` to `approval`:

- An option is **affirmative** if its label, lowercased, begins with / contains an
  approval token (`yes`, `approve`, `allow`, `proceed`, `confirm`, `ok`) and is not
  negated (`no`, `don't`, `cancel`, `reject`, `deny`, `skip`, `keep`, `abort`,
  `esc`).
- An affirmative is **sticky** if it also contains a "standing grant" marker
  (`don't ask again`, `auto-accept`, `always`, `for the rest`, `this session`,
  `this project`, `remember`).
- **Least-privilege rule:** among affirmatives, prefer a **non-sticky** one (the
  plain "Yes" / "Yes, once"). Set `AffirmativeIdx` to that option's 1-based index.
  Only if *every* affirmative is sticky do we set `AffirmativeIdx` to the sticky one
  and `AffirmativeSticky = true`.

Worker change in `tryAutoApprove`:

- If `AffirmativeIdx == 0` → **skip** (no recognizable yes; leave for human). This
  is strictly safer than today, which would have pressed `1` regardless.
- If `AffirmativeSticky` is true and the user has **not** opted into sticky grants →
  **skip** (don't hand out a standing permission). Opt-in is a new config flag,
  `auto_approve_allow_sticky` (default `false`), and/or a per-session equivalent.
- Otherwise send `strconv.Itoa(AffirmativeIdx)` instead of the literal `"1"`.

This is intentionally conservative: the common Claude layout (option 1 = sticky,
option 2 = "Yes", option 3 = "No") now auto-presses **2**, not 1, so we never
silently create a standing grant. When the prompt only offers a sticky yes, we
abstain unless the user explicitly allowed sticky.

### Weakness 2 — Allow/deny policy (replaces the single boolean)

Replace "approve everything when on" with an explicit policy that classifies each
prompt by **tool name** and/or **prompt pattern**, with optional **path scoping**,
evaluated as: **deny wins over allow**.

#### Classification

From an `Approval` we derive:

- **Tool** — parsed from `Action` (e.g. `Bash(rm -rf node_modules)` → tool `Bash`,
  argument `rm -rf node_modules`). Reuse `looksLikeAction` (`approval.go:123`); take
  the substring before the first `(`.
- **Argument / pattern text** — the inside of the parens plus `Question`, matched
  against user-supplied globs/substrings.
- **Path** — when the argument contains a path (Edit/Write/Read targets, or a path
  token in a Bash command), match against `paths:` scopes.

#### Evaluation order (per prompt)

```
Parse(pane) -> ok? ───no──> leave for human
   │ yes
   ▼
built-in destructive deny (Weakness 3) ── match ──> SKIP (never auto-confirm)
   │ no match
   ▼
policy DENY rule match? ── yes ──> SKIP (leave for human)
   │ no
   ▼
policy ALLOW rule match? ── no ──> leave for human (manual)
   │ yes
   ▼
affirmative selection (Weakness 1) ── ok ──> SendKeys(affirmativeIdx)
                                   └─ not ok / sticky-not-opted ──> leave for human
```

"Leave for human" = exactly today's no-op: status stays `waiting_for_input`, the
prompt is still answerable via the approvals inbox / attach.

#### Config schema (allowlist)

Auto-approve becomes a nested block in `~/.warden/config.yaml`. The legacy
`auto_approve: bool` is retained as a coarse master switch (`enabled`) for backward
compatibility and migration.

```yaml
# Auto-approve policy. The daemon auto-answers a recognized permission prompt only
# when it matches an `allow` rule, does NOT match a `deny` rule, and is not on the
# built-in destructive deny-list (which always wins). Tools/patterns are matched
# case-insensitively; paths are matched as globs against the action's target.
auto_approve:
  enabled: false            # master switch (replaces the old auto_approve bool)
  allow_sticky: false       # opt in to pressing "yes, don't ask again" options
  rules:
    allow:
      - tool: Read           # tool-name match (case-insensitive)
      - tool: Edit
        paths: ["src/**", "docs/**"]   # only auto-approve edits under these globs
      - tool: Bash
        pattern: "npm test*"           # arg/question glob; tool optional if pattern given
      - pattern: "*lint*"              # pattern-only rule (any tool)
    deny:
      - tool: Bash
        pattern: "git push*"           # never auto-approve pushes, even if an allow matched
      - tool: WebFetch                 # belt-and-suspenders: keep network egress manual
```

Rule semantics:

- A rule matches when **all** of its present fields match (`tool` AND `pattern` AND
  `paths`). An absent field is a wildcard.
- `paths` matches if the action's extracted path matches **any** glob in the list.
- **deny wins:** if any `deny` rule matches, the prompt is skipped regardless of
  `allow`.
- Empty `allow` ⇒ nothing is auto-approved (safe). `enabled: false` ⇒ feature off
  entirely (current behavior preserved).

#### Per-session override

`Session.AutoApprove bool` is kept for the simple "on/off for this agent" UX and
the existing `warden auto-approve <id> on|off` command, but its meaning becomes
"evaluate this agent against the global policy" rather than "approve everything."
A richer per-session policy is out of scope (see Out of Scope). A new policy type
lives in a small `internal/approval/policy.go` (or `internal/autoapprove`):

```go
type Rule struct {
    Tool    string   `yaml:"tool,omitempty"`
    Pattern string   `yaml:"pattern,omitempty"`
    Paths   []string `yaml:"paths,omitempty"`
}
type Policy struct {
    Enabled     bool   `yaml:"enabled"`
    AllowSticky bool   `yaml:"allow_sticky"`
    Allow       []Rule `yaml:"allow"`
    Deny        []Rule `yaml:"deny"`
}

// Decision is the single entry point the poller calls.
func (p Policy) Decide(a approval.Approval) Decision // Approve(idx)/Skip(reason)
```

This keeps the poller thin: `tryAutoApprove` calls `policy.Decide(a)` and either
`SendKeys` the returned index or logs the skip reason.

### Weakness 3 — Built-in destructive deny-list (hard guard)

Independent of any user policy, maintain a compiled-in deny-list that inspects the
**Action**, **Question**, and the **affirmative option's own label** for markers of
irreversible / outward-facing actions. If any marker is present, the prompt is
**never** auto-approved — this check runs *first* and wins over every allow rule, so
no config mistake (and no sticky option-1 grant) can auto-confirm something
destructive.

Markers (case-insensitive, word-boundary aware to limit false matches):

```
delete, remove, rm -rf, force, --force, -f (git), overwrite, truncate, drop,
push, publish, deploy, release, reset --hard, prune, destroy, wipe, purge,
revoke, format
```

Implementation: `approval.IsDestructive(a Approval) (bool, string)` returning the
matched marker for logging. The guard is conservative on purpose — a false positive
just means "ask the human," which is always safe. Markers are a `var` slice so tests
can assert membership and the list can grow without touching the poller.

Worker order (final):

```go
a, ok := approval.Parse(pane)
if !ok || len(a.Options) == 0 { /* skip: unrecognized */ return }
if bad, marker := approval.IsDestructive(a); bad {
    log.Printf("auto-approve BLOCKED (destructive: %q) for %s", marker, s.ID); return
}
d := p.policy.Decide(a)              // Stage B; Stage A: implicit allow-all
if d.Skip { log.Printf("auto-approve skipped for %s: %s", s.ID, d.Reason); return }
if a.AffirmativeIdx == 0 { return }  // no recognizable yes
if a.AffirmativeSticky && !p.policy.AllowSticky { return }
p.deps.SendKeys(ctx, s.TmuxSession, strconv.Itoa(a.AffirmativeIdx))
```

## Staged Rollout

### Stage A — Safe default (no schema change)

Ships weaknesses **1** and **3**. Scope:

- `approval`: add `AffirmativeIdx` / `AffirmativeSticky` + `classifyOptions`, and
  `IsDestructive` + the marker list.
- `poller.tryAutoApprove`: send the affirmative index (not `1`); run the destructive
  guard first; abstain on sticky-only / no-affirmative prompts.
- Config: add a single `auto_approve_allow_sticky` bool (default `false`) to the
  existing flat schema. **No nested block yet** — `auto_approve: bool` keeps working
  exactly as today as the master switch.

Outcome: the *existing* boolean becomes safe — auto-approve now means "press the
least-privilege Yes on a recognized, non-destructive prompt," instead of "press 1 on
anything." This is the safety-critical, low-surface-area change and can land on its
own.

### Stage B — Selective policy (schema change)

Ships weakness **2**. Scope:

- Introduce the nested `auto_approve` block (`enabled`, `allow_sticky`, `rules`) and
  the `Policy`/`Rule` types + `Decide`. Migrate the flat `auto_approve: bool` →
  `auto_approve.enabled` and `auto_approve_allow_sticky` → `auto_approve.allow_sticky`
  during `config.Reconcile` (add nested keys honoring the old values; per the
  config-file design, Reconcile only adds missing keys and preserves existing ones).
- Tool/pattern/path classification helpers in `approval`.
- `poller`: thread the loaded `Policy` into the `Poller`; `tryAutoApprove` calls
  `Decide`.

Outcome: auto-approve is genuinely selective — "auto-approve Read/Edit under
`src/**`, never Bash pushes." The Stage-A destructive guard still runs first and
still wins.

## Test Plan

### `internal/approval` (unit)

- **`classifyOptions`**: table-driven over real Claude layouts —
  - `["Yes","No"]` → AffirmativeIdx 1, not sticky.
  - `["Yes, and don't ask again for Bash","Yes","No"]` → AffirmativeIdx **2**, not
    sticky (least-privilege beats sticky).
  - `["Yes, allow always","No, keep asking"]` → AffirmativeIdx 1, **sticky** (only
    affirmative is sticky).
  - `["No","Cancel"]` / multi-select-ish → AffirmativeIdx 0 (abstain).
  - negation traps: `"No, and don't ask again"` is **not** affirmative.
- **`IsDestructive`**: positives for `Bash(rm -rf build)`, `git push --force`,
  `Bash(git reset --hard)`, "publish to npm", "Overwrite existing file?",
  affirmative-label-only matches ("Yes, delete it"); negatives for `Read`,
  `Edit(src/x.go)`, "Yes / No", and near-miss words that shouldn't trip word-boundary
  matching (e.g. "removed" in prose vs. action) as far as the heuristic allows.
- **Tool/path extraction** (Stage B): `Bash(npm test)` → tool `Bash`, arg `npm
  test`; `Edit(src/a.go)` → tool `Edit`, path `src/a.go`; non-action `Action` → empty
  tool.

### `internal/approval` Policy (`Decide`) (unit, Stage B)

- deny-wins: prompt matches both an allow and a deny → Skip.
- allow + path scope: `Edit(src/a.go)` allowed, `Edit(secrets/.env)` not allowed.
- pattern-only allow matches across tools; tool-only allow ignores args.
- empty allow ⇒ Skip everything; `enabled:false` ⇒ Skip everything.
- destructive marker present but also matches an allow rule ⇒ still blocked (guard
  precedes `Decide`, asserted at the poller level).

### `internal/poller` (`tryAutoApprove`, with mock `Deps`)

- Sends the **affirmative index**, not always `1`, given a sticky-first layout
  (assert `SendKeys` arg == `"2"`).
- Destructive prompt → **no** `SendKeys` call; logs BLOCKED.
- Sticky-only affirmative with `allow_sticky=false` → no `SendKeys`; with `true` →
  presses sticky index.
- `AffirmativeIdx==0` → no `SendKeys`.
- Unrecognized prompt (Parse !ok) → unchanged no-op.
- Stage B: policy deny → no `SendKeys`; policy allow + non-destructive + affirmative
  → `SendKeys` with correct index.
- `SendKeys` error path still leaves the session `waiting_for_input` (unchanged).

### `internal/config`

- Stage A: `auto_approve_allow_sticky` parse (default false; on/off).
- Stage B: Reconcile migrates flat `auto_approve: true` →
  `auto_approve.enabled: true` while preserving an existing nested block and unknown
  keys; drift-guard test updated for the nested shape.

### Integration / manual

- Spawn an agent that triggers a sticky-style Bash prompt with auto-approve on;
  verify warden presses the one-shot Yes (no standing grant created on the next
  identical prompt — it prompts again).
- Trigger a `git push --force` permission prompt with a permissive allow policy;
  verify warden **does not** answer and the prompt surfaces in the inbox.
- Stage B: configure allow `Read`/`Edit` only; verify a `Bash` prompt is left for a
  human while reads/edits proceed.

## Backward Compatibility & Safety

- Stage A changes only *which* key is pressed and *whether* to abstain — it never
  presses something today's code wouldn't have; in every divergence it is *more*
  conservative (abstain or pick the non-sticky yes). Existing `auto_approve: bool`
  configs keep working.
- Stage B is gated behind the nested schema; until a user writes `allow` rules,
  `enabled:true` with an empty allow approves nothing (fail-safe), which is a
  behavior change from "approve everything" → documented in the migration note and
  the config hint comment.
- The destructive deny-list is compiled in and unconditional; there is deliberately
  no config switch to disable it.

## Out of Scope

- Per-session allow/deny rule sets (only the global policy + per-session on/off).
- Regex rules (globs/substrings only, to keep config approachable and matching
  fast).
- Learning/auto-suggesting allow rules from a user's manual-approval history.
- Surfacing policy decisions in the TUI/Web beyond existing logs (a future
  observability pass; cf. the metrics endpoint note in the original design).

## Open Questions

- Marker tuning for `IsDestructive`: start with the conservative list above and grow
  it from real false-negatives; false positives are harmless (they just ask the
  human). Worth a follow-up once Stage A has run on real workloads.
- Whether `WebFetch`/network tools should be destructive-listed by default or left
  to user deny rules — leaning "user deny rule" (Stage B) since fetches are not
  irreversible, but flagged here for review.
