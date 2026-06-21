# Auto-Approve Hardening — Stage A Implementation Plan

**Date:** 2026-06-21
**Status:** Ready to implement
**Design:** 2026-06-21-auto-approve-hardening-design.md (Stage A)

## Scope

Stage A makes the *existing* `auto_approve` boolean safe by default, with **no
nested config schema** (that is Stage B). It ships two design weaknesses:

- **#1 Affirmative-option selection** — stop pressing a hardcoded `1`; press the
  least-privilege "Yes", and abstain on sticky-only / no-affirmative prompts unless
  the user opts into sticky grants.
- **#3 Built-in destructive deny-list** — never auto-confirm a recognized prompt
  whose action/question/affirmative-label names an irreversible or outward-facing
  operation; this guard runs first and is not configurable.

One new flat config key, `auto_approve_allow_sticky` (default `false`), is added to
the existing schema table.

**Out of scope (Stage B):** the nested `auto_approve.{enabled,allow_sticky,rules}`
block, tool/pattern/path classification, `Policy.Decide`. This plan must not touch
`Session.AutoApprove` semantics or the `warden auto-approve` command.

## Files touched

| File | Change |
|---|---|
| `internal/approval/approval.go` | add `AffirmativeIdx`/`AffirmativeSticky` fields + `classifyOptions`; add `IsDestructive` + marker list |
| `internal/approval/approval_test.go` | unit tests for both new functions |
| `internal/approval/testdata/*.txt` | 1–2 new fixtures (sticky-first, destructive) |
| `internal/poller/poller.go` | rewrite `tryAutoApprove` body; add `AutoApproveAllowSticky bool` field |
| `internal/poller/poller_test.go` | tests for the new `tryAutoApprove` branches |
| `internal/config/config.go` | add `AutoApproveAllowSticky` field, default, schema entry, legacy-env name |
| `internal/config/config_test.go` | extend defaults/drift-guard coverage |
| `internal/cli/daemon.go` | wire `pl.AutoApproveAllowSticky = cfg.AutoApproveAllowSticky` |

No store, API, or CLI-command changes.

## Step 1 — `internal/approval`: affirmative classification

In `approval.go`, extend the `Approval` struct (`approval.go:14`):

```go
type Approval struct {
    Action            string
    Question          string
    Options           []string
    SelectedIdx       int
    AffirmativeIdx    int  // 1-based least-privilege "yes"; 0 = none found
    AffirmativeSticky bool // true when the chosen affirmative is a standing/"don't ask again" grant
}
```

Add a classifier and wire it into `Parse` just before the `return a, true`
(`approval.go:102`), so every parsed prompt is classified once:

```go
a.AffirmativeIdx, a.AffirmativeSticky = classifyOptions(a.Options)
```

`classifyOptions`:

- Lowercase each label. An option is **affirmative** if it contains an approval
  token (`yes`, `approve`, `allow`, `proceed`, `confirm`) **and** does not start
  with / contain a negation token (`no`, `n't`, `cancel`, `reject`, `deny`, `skip`,
  `keep asking`, `abort`). Note the real fixture
  `"Yes, and always allow access to tmp/ from this project"` must classify as
  affirmative+sticky, while `"No"` must not.
- An affirmative is **sticky** if it also contains a standing-grant marker
  (`don't ask again`, `always`, `auto-accept`, `for the rest`, `this session`,
  `this project`, `remember`).
- **Least-privilege:** scan affirmatives; if any non-sticky affirmative exists,
  return its 1-based index with `sticky=false`. Otherwise, if any (sticky)
  affirmative exists, return the first one's index with `sticky=true`. If none,
  return `(0, false)`.

Keep the token lists as package-level `var` slices so tests can reference them and
they can grow without touching `Parse`.

Note `BuildView` (`approval.go:147`) does not need the new fields — leave `View`
unchanged.

## Step 2 — `internal/approval`: destructive deny-list

Add to `approval.go`:

```go
// destructiveMarkers are lowercase substrings that mark an irreversible or
// outward-facing action. Matching any one blocks auto-approval unconditionally.
var destructiveMarkers = []string{
    "delete", "remove", "rm -rf", "force", "--force", "overwrite", "truncate",
    "drop", "push", "publish", "deploy", "release", "reset --hard", "prune",
    "destroy", "wipe", "purge", "revoke", "format",
}

// IsDestructive reports whether a's action, question, or chosen affirmative label
// names a destructive operation. The matched marker is returned for logging.
func IsDestructive(a Approval) (bool, string) { ... }
```

Match case-insensitively across `a.Action`, `a.Question`, and — when
`a.AffirmativeIdx > 0` — `a.Options[a.AffirmativeIdx-1]`. Return the first marker
hit. Conservative by design: a false positive only means "ask the human."

Keep matching as plain `strings.Contains` on lowercased text for Stage A (simple,
fast, predictable). Word-boundary refinement is a noted follow-up in the design and
not required here.

## Step 3 — `internal/poller`: rewrite `tryAutoApprove`

Add a field to `Poller` (near `AutoApproveGlobal`, `poller.go:112`):

```go
// AutoApproveAllowSticky lets auto-approve press a sticky "don't ask again"
// affirmative. Off by default so unattended approval never grants standing perms.
AutoApproveAllowSticky bool
```

Replace the body of `tryAutoApprove` (`poller.go:169`) after the existing gate and
`Parse` (keep `poller.go:171` gate and `poller.go:176-180` unrecognized check
verbatim) with:

```go
if bad, marker := approval.IsDestructive(a); bad {
    log.Printf("auto-approve BLOCKED for %s: destructive (%q)", s.ID, marker)
    return
}
if a.AffirmativeIdx == 0 {
    log.Printf("auto-approve skipped for %s: no affirmative option", s.ID)
    return
}
if a.AffirmativeSticky && !p.AutoApproveAllowSticky {
    log.Printf("auto-approve skipped for %s: only a sticky affirmative (allow_sticky off)", s.ID)
    return
}
key := strconv.Itoa(a.AffirmativeIdx)
if err := p.deps.SendKeys(ctx, s.TmuxSession, key); err != nil {
    log.Printf("auto-approve failed for %s: %v", s.ID, err)
    return
}
log.Printf("auto-approved %s -> option %s: %s", s.ID, key, a.Options[a.AffirmativeIdx-1])
if p.OnChange != nil {
    p.OnChange()
}
```

Add `"strconv"` to the imports. Update the doc comment on `tryAutoApprove`
(`poller.go:162-168`) to describe affirmative selection + the destructive guard
instead of "sending option 1". The `Deps.SendKeys` contract (`poller.go:76-77`) is
unchanged.

## Step 4 — `internal/config`: the `auto_approve_allow_sticky` key

Three coordinated edits (the drift-guard test enforces all three stay in sync):

1. `Config` struct (`config.go:34`, beside `AutoApproveEnabled`):
   `AutoApproveAllowSticky bool \`yaml:"auto_approve_allow_sticky"\``
2. `defaults()` (`config.go:109`): `AutoApproveAllowSticky: false,`
3. `schema` slice (`config.go:75`, right after the `auto_approve` entry):
   ```go
   {"auto_approve_allow_sticky", "When auto-approving, also accept \"yes, don't ask again\" (sticky) options. Values: true | false"},
   ```

Add `AUTO_APPROVE_ALLOW_STICKY` to `legacyEnvNames` (`config.go:374`) for the
ignored-env warning's completeness. No validation rule needed (a bare bool).
`Reconcile` will add the key to existing files automatically (add-missing path,
`config.go:267`), preserving user values.

## Step 5 — wire config → poller

In `internal/cli/daemon.go`, directly after the existing
`pl.AutoApproveGlobal = cfg.AutoApproveEnabled` (`daemon.go:83`):

```go
pl.AutoApproveAllowSticky = cfg.AutoApproveAllowSticky
```

## Test plan

### `internal/approval/approval_test.go`

- **`classifyOptions`** table test:
  - `["Yes","No"]` → `(1, false)`.
  - existing fixture order `["Yes","Yes, and always allow access to tmp/ from this project","No"]`
    → `(1, false)` (non-sticky "Yes" wins, option 1 here).
  - sticky-first `["Yes, and don't ask again for Bash commands","Yes","No"]`
    → `(2, false)` (least-privilege picks the plain Yes at index 2).
  - sticky-only `["Yes, allow always","No, keep asking"]` → `(1, true)`.
  - `["No","Cancel"]` → `(0, false)`.
  - negation trap `["No, and don't ask again","Yes"]` → `(2, false)` (the "No…"
    option must not be treated as affirmative).
- **`IsDestructive`** table test:
  - positives: `Action:"Bash(rm -rf build)"`, `Action:"Bash(git push --force)"`,
    `Action:"Bash(git reset --hard)"`, `Question:"Overwrite existing file?"`,
    affirmative-label `"Yes, delete it"` (set `AffirmativeIdx`).
  - negatives: `Action:"Read(src/x.go)"`, `Action:"Edit(src/x.go)"`,
    options `["Yes","No"]` with benign question.
- Add fixtures only if you prefer end-to-end `Parse`→classify coverage; otherwise
  drive `classifyOptions`/`IsDestructive` directly with literals (lighter, matches
  the table style above). One captured sticky-first fixture
  (`testdata/bash_prompt_sticky_first.txt`) is worth adding as a `Parse` regression
  that asserts `AffirmativeIdx==2`.

### `internal/poller/poller_test.go`

Reuse the existing `stubDeps` SendKeys recorder (`poller_test.go:105-112`). Drive
`tryAutoApprove` (or publish an `ApprovalEvent`) with crafted panes / sessions:

- **Sticky-first pane** with `AutoApprove` on → asserts `SendKeys` called once with
  `"2"` (not `"1"`).
- **Destructive pane** (`rm -rf` / `git push`) with auto-approve on → asserts
  **zero** `SendKeys` calls.
- **Sticky-only affirmative**: `AutoApproveAllowSticky=false` → zero SendKeys;
  `=true` → one SendKeys with the sticky index.
- **No affirmative** (`["No","Cancel"]`) → zero SendKeys.
- **Gate off** (`AutoApproveGlobal=false`, `s.AutoApprove=false`) → zero SendKeys
  (unchanged).
- **Unrecognized pane** → zero SendKeys (unchanged).

Build panes from the same `testdata` fixtures the approval package uses, or inline
minimal option boxes; the poller test already constructs panes for classify tests.

### `internal/config/config_test.go`

- Defaults test asserts `AutoApproveAllowSticky == false`.
- The reflection **drift-guard** test (per the config-file design) will fail unless
  the struct tag and schema entry are both present — this is the safety net for
  Step 4; just ensure it runs.
- A reconcile test: an existing file without `auto_approve_allow_sticky` gains the
  key (with default `false`) while keeping other values/comments.

## Implementation order & verification

1. `internal/approval` (Steps 1–2) + its tests — self-contained, no other package
   depends on the new fields yet.
2. `internal/config` (Step 4) + tests — drift-guard confirms wiring.
3. `internal/poller` (Step 3) + tests — depends on Step 1.
4. `internal/cli/daemon.go` (Step 5) — one line.

Verify:

```
go test ./internal/approval/... ./internal/poller/... ./internal/config/...
go build ./...
go vet ./...
```

## Risks / notes

- **Behavior change for existing users:** with `auto_approve: true`, a prompt that
  offers *only* a sticky affirmative now **abstains** (previously it pressed `1`).
  This is the intended safety improvement; call it out in the changelog. Users who
  want the old "accept sticky" behavior set `auto_approve_allow_sticky: true`.
- **Affirmative heuristic false-negatives** (abstaining on an oddly-worded "Yes")
  are safe — the prompt simply waits for a human. Grow the token lists from real
  captures rather than guessing.
- Keep `IsDestructive` independent of any config flag — there is deliberately no
  switch to disable it (design §Weakness 3).
