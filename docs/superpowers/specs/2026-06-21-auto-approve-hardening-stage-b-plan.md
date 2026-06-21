# Auto-Approve Hardening — Stage B Implementation Plan

**Date:** 2026-06-21
**Status:** Ready to implement (after Stage A merges)
**Design:** 2026-06-21-auto-approve-hardening-design.md (Stage B)
**Builds on:** 2026-06-21-auto-approve-hardening-stage-a-plan.md (must be merged first)

## Scope

Stage B makes auto-approve **selective** (design Weakness 2): replace the single
master toggle with an allow/deny **policy** keyed on tool name, prompt pattern, and
path scope, evaluated as **deny wins over allow**. The built-in destructive guard
and affirmative-selection logic from Stage A still run first and still win.

Concretely Stage B:

- Adds a `Policy`/`Rule` type and a `Decide` rule-matcher in `internal/approval`,
  plus tool/argument/path classification helpers and a glob matcher.
- Turns the flat `auto_approve: bool` config key into a **nested block**
  (`auto_approve.{enabled, allow_sticky, rules.{allow, deny}}`), folding in the
  Stage-A `auto_approve_allow_sticky` flat key. `config.Reconcile` migrates existing
  files in place; `config.Load` tolerates both shapes.
- Threads the loaded `Policy` into the poller; `tryAutoApprove` calls `Decide`
  between the destructive guard and the affirmative send.

**Out of scope:** per-session allow/deny rule sets (per-session stays a bare on/off
that opts the agent into the global policy), regex rules (globs/substrings only),
TUI/Web surfacing of policy decisions, and any change to the destructive deny-list
shipped in Stage A.

## Baseline assumption

This plan is grounded against **main + the merged Stage A branch**. Stage A already
added to the tree:

- `approval.Approval.AffirmativeIdx` / `AffirmativeSticky`, `classifyOptions`,
  `IsDestructive` + `destructiveMarkers`.
- `config.AutoApproveAllowSticky bool` (`yaml:"auto_approve_allow_sticky"`), its
  default, its schema entry, and `AUTO_APPROVE_ALLOW_STICKY` in `legacyEnvNames`.
- `poller.Poller.AutoApproveAllowSticky bool`, and a `tryAutoApprove` that runs the
  destructive guard, abstains on no-affirmative / sticky-only, and sends
  `strconv.Itoa(a.AffirmativeIdx)`.
- `daemon.go`: `pl.AutoApproveAllowSticky = cfg.AutoApproveAllowSticky`.

Stage B **rewrites several of those Stage-A additions** (the flat sticky key becomes
a nested field; the two poller bool fields become one `Policy` field). That overlap
on `config.go` and `poller.go` is exactly why this stage must run *after* Stage A
merges, not in parallel with it.

## Key design decisions (resolved here, not left open)

1. **`Policy`/`Rule`/`Decide` live in `internal/approval`** (`approval/policy.go`),
   carrying yaml tags, per the design. `approval` stays a near-leaf package; it gains
   a `gopkg.in/yaml.v3` import solely for a legacy-bool `UnmarshalYAML` shim (below).
   `config` imports `approval` (acyclic: `approval` does not import `config`).
2. **`Decide` is a pure rule-matcher** — it evaluates deny/allow only and never
   inspects `Enabled`, the destructive markers, or the affirmative index. The poller
   owns the participate-gate (`Enabled` ∥ per-session), the destructive guard (runs
   *before* `Decide`), and the affirmative/sticky send (Stage A code, unchanged).
   This keeps each concern in one place and matches the design's final worker order.
   `Decide` returns a small `Decision{Approve bool, Reason string}` (no index — the
   index already lives on `Approval.AffirmativeIdx`).
3. **`config.Load` tolerates both file shapes** via a custom `UnmarshalYAML` on
   `Policy` that accepts either a scalar bool (legacy `auto_approve: true` →
   `Enabled`) or a mapping (the new block). This means Load never hard-fails on an
   un-migrated file, independent of Reconcile ordering. `Reconcile` still rewrites
   the file to the nested shape so the `rules:` block is visible for the user to edit.
4. **The drift-guard stays flat and unchanged in spirit.** `auto_approve` remains a
   *single top-level key* whose value is now a mapping; the nested fields are not
   top-level schema entries. The reflection test still asserts "top-level yaml tags
   == schema keys"; we only remove the now-nested `auto_approve_allow_sticky` entry.

## Files touched

| File | Change |
|---|---|
| `internal/approval/policy.go` *(new)* | `Policy`, `Rule`, `Decision`, `Decide`; `toolOf`/`argOf`/`pathsOf` classification; `globMatch`; `Policy.UnmarshalYAML` legacy-bool shim |
| `internal/approval/policy_test.go` *(new)* | `Decide` table, classification, glob, legacy-bool unmarshal |
| `internal/config/config.go` | replace `AutoApproveEnabled`+`AutoApproveAllowSticky` with nested `AutoApprove approval.Policy`; update `defaults()`, `schema` entry/hint, drop the flat sticky schema entry; add `migrateAutoApprove` to `Reconcile` |
| `internal/config/config_test.go` | drift-guard (one top-level `auto_approve`), legacy-bool Load, nested Load, Reconcile migration |
| `internal/poller/poller.go` | replace `AutoApproveGlobal`+`AutoApproveAllowSticky` fields with `AutoApprovePolicy approval.Policy`; insert `Decide` call in `tryAutoApprove`; update the participate-gate + sticky check |
| `internal/poller/poller_test.go` | policy deny / allow / destructive-still-wins / per-session opt-in cases |
| `internal/cli/daemon.go` | replace the two `pl.AutoApprove*` wiring lines with `pl.AutoApprovePolicy = cfg.AutoApprove` |

No store, API, or CLI-command changes. `Session.AutoApprove bool` keeps its field and
the `warden auto-approve <id> on|off` command; only its *meaning* shifts to "evaluate
this agent against the global policy."

## Step 1 — `internal/approval/policy.go`: the policy engine

### Types

```go
// Rule matches a parsed prompt. A present field must match; an absent field is a
// wildcard. tool/pattern are case-insensitive; paths are globs against the action
// target. A rule matches when ALL of its present fields match.
type Rule struct {
    Tool    string   `yaml:"tool,omitempty"`
    Pattern string   `yaml:"pattern,omitempty"`
    Paths   []string `yaml:"paths,omitempty"`
}

// Rules is the allow/deny pair under auto_approve.rules.
type Rules struct {
    Allow []Rule `yaml:"allow"`
    Deny  []Rule `yaml:"deny"`
}

// Policy is the global auto-approve policy loaded from config.
type Policy struct {
    Enabled     bool  `yaml:"enabled"`
    AllowSticky bool  `yaml:"allow_sticky"`
    Rules       Rules `yaml:"rules"`
}

// Decision is the result of evaluating a prompt against the allow/deny rules only.
type Decision struct {
    Approve bool
    Reason  string // why it was skipped (for logging); "" when Approve
}
```

### `Decide`

```go
// Decide evaluates a against the allow/deny rules. It does NOT check Enabled, the
// destructive deny-list, or the affirmative index — the caller (poller) owns those.
// Empty Allow => approve nothing (fail-safe). deny wins over allow.
func (p Policy) Decide(a Approval) Decision {
    tool := toolOf(a)
    arg := argOf(a)
    paths := pathsOf(a)
    for _, r := range p.Rules.Deny {
        if r.matches(tool, arg, a.Question, paths) {
            return Decision{false, "matched a deny rule"}
        }
    }
    for _, r := range p.Rules.Allow {
        if r.matches(tool, arg, a.Question, paths) {
            return Decision{Approve: true}
        }
    }
    return Decision{false, "no allow rule matched"}
}
```

`(r Rule) matches(tool, arg, question string, paths []string) bool`:

- `r.Tool != ""` ⇒ `strings.EqualFold(r.Tool, tool)` must hold.
- `r.Pattern != ""` ⇒ `globMatch(r.Pattern, tool+"("+arg+")")` **or**
  `globMatch(r.Pattern, question)` (case-insensitive) must hold.
- `len(r.Paths) > 0` ⇒ at least one `globMatch(glob, candidate)` over the extracted
  `paths` must hold.
- An empty rule (`Rule{}`) matches everything — call this out in a doc comment; it is
  a foot-gun in `allow` (approves all non-destructive prompts) and intentional in
  `deny` only if someone really wants a kill-switch.

### Classification helpers

```go
// toolOf returns the tool name from a.Action ("Bash(rm -rf x)" -> "Bash"); "" when
// Action is not a Tool(...) header. Reuses the looksLikeAction shape.
func toolOf(a Approval) string

// argOf returns the parenthesized argument ("Bash(rm -rf x)" -> "rm -rf x"); "" if
// none.
func argOf(a Approval) string

// pathsOf returns candidate path tokens from the argument: the whole argument plus
// any whitespace-separated token containing '/' or '.' (covers Edit/Write/Read
// targets and path-bearing Bash args). De-duplicated; empty slice when none.
func pathsOf(a Approval) []string
```

`toolOf`/`argOf` split `a.Action` on the first `(`; `argOf` trims the trailing `)`.
Keep them defensive (no panic on malformed input) and reuse `looksLikeAction`
(`approval.go:123`) to decide whether `Action` is an action header at all.

### Glob matcher

`path.Match` does not support `**`, which the design's path scopes (`src/**`) need.
Add a small `globMatch(pattern, s string) bool`:

- Lowercase both (case-insensitive).
- If `pattern` contains no glob metacharacter (`* ? [`), match as a **substring**
  (`strings.Contains`) — this is what makes `pattern: "git push"` behave intuitively.
- Otherwise translate the glob to an anchored regexp **once** (cache compiled
  patterns in a small `sync.Map` keyed by the raw glob): `**` → `.*`, `*` →
  `[^/]*`, `?` → `[^/]`, escape the rest. Match against the full string.

Keep this matcher self-contained and unit-tested; it is the part most likely to
surprise users.

### Legacy-bool `UnmarshalYAML`

```go
// UnmarshalYAML accepts either a scalar bool (legacy `auto_approve: true`, mapped to
// Enabled) or the nested mapping. Lets config.Load read both shapes without failing.
func (p *Policy) UnmarshalYAML(node *yaml.Node) error {
    if node.Kind == yaml.ScalarNode {
        var b bool
        if err := node.Decode(&b); err != nil {
            return err
        }
        p.Enabled = b
        return nil
    }
    type raw Policy // avoid recursion
    var r raw
    if err := node.Decode(&r); err != nil {
        return err
    }
    *p = Policy(r)
    return nil
}
```

This is the only reason `approval` imports `gopkg.in/yaml.v3`.

## Step 2 — `internal/config/config.go`: nested block + migration

### Struct, defaults, schema

1. In `Config` (`config.go:34`), replace the two flat fields
   (`AutoApproveEnabled bool` and the Stage-A `AutoApproveAllowSticky bool`) with:
   ```go
   AutoApprove approval.Policy `yaml:"auto_approve"`
   ```
   Add the `internal/approval` import.
2. In `defaults()` (`config.go:109`), replace the two flat defaults with:
   ```go
   AutoApprove: approval.Policy{
       Enabled:     false,
       AllowSticky: false,
       Rules:       approval.Rules{Allow: []approval.Rule{}, Deny: []approval.Rule{}},
   },
   ```
   Use non-nil empty slices so a generated file renders `allow: []` / `deny: []`
   (clear edit points) rather than `null`.
3. In `schema` (`config.go:69`): keep one `auto_approve` entry, **remove** the
   Stage-A `auto_approve_allow_sticky` entry, and rewrite the `auto_approve` hint to
   document the nested policy, e.g.:
   ```go
   {"auto_approve", "Auto-approve policy. The daemon answers a recognized prompt only when it matches an allow rule, matches no deny rule, and is not on the built-in destructive deny-list (which always wins). Sub-keys: enabled (master switch), allow_sticky (press \"don't ask again\" options), rules.allow / rules.deny (lists of {tool, pattern, paths})."},
   ```
   (Single head-comment on the top-level key. Per-subkey comments are optional polish
   — see Risks.)
4. In `legacyEnvNames` (`config.go:374`), drop `AUTO_APPROVE_ALLOW_STICKY` (it never
   shipped as a real env var) and keep `AUTO_APPROVE`.

Because `defaultsMapping()` (`config.go:313`) marshals `defaults()` through yaml, the
nested `AutoApprove` struct already renders as a nested mapping in both `renderFull`
and `defaultValueNodes` with no other changes — a fully-absent `auto_approve` key is
re-added as the complete block automatically.

### Migration in `Reconcile`

The add-missing loop in `Reconcile` (`config.go:267`) sees `auto_approve` as
"present" and skips it even when its value is a legacy scalar — so a dedicated
migration must run first. In `Reconcile`, immediately after obtaining `mapping`
(`config.go:256`) and **before** building the `present` map (`config.go:258`), call:

```go
if migrateAutoApprove(mapping) {
    changed = true // ensure the file is rewritten even if nothing else is missing
}
```

(Hoist `changed` above the loop, or have `migrateAutoApprove` report it and OR it in
before the final `if !changed`.)

`migrateAutoApprove(mapping *yaml.Node) bool`:

1. Find the `auto_approve` key/value pair and the `auto_approve_allow_sticky`
   key/value pair (if any) by scanning `mapping.Content` two at a time.
2. If `auto_approve`'s value node is a `ScalarNode` (legacy bool):
   - Parse it as bool (`enabled`).
   - Read `auto_approve_allow_sticky`'s bool if that key exists (`allow_sticky`),
     else `false`.
   - Build a `MappingNode` value: `enabled: <bool>`, `allow_sticky: <bool>`,
     `rules:` → `{allow: [], deny: []}` (empty sequence nodes). Preserve the existing
     `auto_approve` key node (and its head-comment) — only swap its value node.
   - Remove the `auto_approve_allow_sticky` key+value pair from `mapping.Content`.
   - Return `true`.
3. If `auto_approve`'s value is already a `MappingNode`, but a stray top-level
   `auto_approve_allow_sticky` still exists (partial migration), fold it in (set
   `allow_sticky` inside the block if absent) and remove the stray key; return `true`.
4. Otherwise return `false` (already migrated / nothing to do — the normal
   add-missing path then tops up any absent sub-keys is **not** automatic for nested
   keys; see note).

**Note on nested top-up:** the existing add-missing loop only operates on top-level
keys, so it will *not* add a missing `rules.deny` inside an existing
`auto_approve:` block. For Stage B that is acceptable — `Load` supplies struct
defaults for any missing sub-key, so behavior is correct even if the on-disk block is
sparse. If we want the on-disk block kept complete, that is a small follow-up
(recursively reconcile the `auto_approve` mapping against the default block); call it
out but do not block Stage B on it.

Keep all yaml.Node construction faithful to the existing helpers' style (scalar nodes
with `Tag: "!!bool"` / `"!!str"`, sequence nodes `Kind: yaml.SequenceNode`,
`Tag: "!!seq"`).

## Step 3 — `internal/poller/poller.go`: call `Decide`

1. Replace the Stage-A/legacy fields `AutoApproveGlobal bool` (`poller.go:112`) and
   `AutoApproveAllowSticky bool` with one field:
   ```go
   // AutoApprovePolicy is the global allow/deny policy (from config). Per-session
   // Session.AutoApprove opts an agent into evaluation against this policy.
   AutoApprovePolicy approval.Policy
   ```
2. Rewrite `tryAutoApprove` to the design's final worker order. Keeping the Stage-A
   destructive guard and affirmative/sticky handling, insert the `Decide` gate
   between them:
   ```go
   // participate-gate: global master switch OR per-session opt-in.
   if !p.AutoApprovePolicy.Enabled && !s.AutoApprove {
       return
   }
   a, ok := approval.Parse(pane)
   if !ok || len(a.Options) == 0 {
       log.Printf("auto-approve skipped for %s: unrecognized prompt", s.ID)
       return
   }
   if bad, marker := approval.IsDestructive(a); bad {
       log.Printf("auto-approve BLOCKED for %s: destructive (%q)", s.ID, marker)
       return
   }
   if d := p.AutoApprovePolicy.Decide(a); !d.Approve {
       log.Printf("auto-approve skipped for %s: %s", s.ID, d.Reason)
       return
   }
   if a.AffirmativeIdx == 0 {
       log.Printf("auto-approve skipped for %s: no affirmative option", s.ID)
       return
   }
   if a.AffirmativeSticky && !p.AutoApprovePolicy.AllowSticky {
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
   Note the destructive guard runs **before** `Decide`, so no allow rule can ever
   un-block a destructive prompt (design §Weakness 3). Update the `tryAutoApprove` doc
   comment to describe the policy gate.

The `Deps.SendKeys` contract (`poller.go:76-77`) is unchanged.

## Step 4 — `internal/cli/daemon.go`: wire the policy

Replace the Stage-A pair of lines
(`pl.AutoApproveGlobal = cfg.AutoApproveEnabled` and
`pl.AutoApproveAllowSticky = cfg.AutoApproveAllowSticky`, near `daemon.go:83`) with a
single line:

```go
pl.AutoApprovePolicy = cfg.AutoApprove
```

## Test plan

### `internal/approval/policy_test.go`

- **`globMatch`**: `git push` substring matches `Bash(git push origin main)`; `src/**`
  matches `src/a/b.go` but not `lib/x.go`; `*.go` matches `main.go`; `?` /
  case-insensitivity; a plain non-glob string is a substring match.
- **classification**: `Bash(rm -rf build)` → tool `Bash`, arg `rm -rf build`;
  `Edit(src/a.go)` → tool `Edit`, paths includes `src/a.go`; non-action `Action`
  (e.g. `"Do you want to proceed?"`) → empty tool, empty paths.
- **`Decide`** table:
  - deny-wins: a prompt matching both an allow and a deny rule → `Approve=false`.
  - allow + path scope: `Edit(src/a.go)` with allow `{Tool:Edit, Paths:[src/**]}` →
    approve; `Edit(secrets/.env)` → skip ("no allow rule matched").
  - pattern-only allow matches across tools; tool-only allow ignores args.
  - empty `Allow` ⇒ skip everything; empty `Rule{}` in allow ⇒ approves any prompt
    (documented foot-gun) — assert the behavior so it can't change silently.
  - `Decide` does **not** look at `Enabled` (assert an `Enabled:false` policy with a
    matching allow still returns `Approve=true` — the gate is the poller's job).
- **`UnmarshalYAML`**: decoding `auto_approve: true` (scalar) yields
  `Policy{Enabled:true}`; decoding the nested mapping yields the full struct;
  decoding a malformed scalar errors.

### `internal/poller/poller_test.go`

Reuse the existing `stubDeps` SendKeys recorder.

- policy deny rule matches the pane → **zero** SendKeys.
- policy allow + non-destructive + non-sticky affirmative → one SendKeys with the
  correct affirmative index (reuse the Stage-A sticky-first fixture: assert `"2"`).
- **destructive still wins:** a `git push --force` / `rm -rf` pane with an allow rule
  that *would* match → still zero SendKeys (guard precedes `Decide`).
- empty allow with `Enabled:true` → zero SendKeys.
- per-session opt-in: `Enabled:false` policy with allow rules + `s.AutoApprove=true`
  + matching benign prompt → one SendKeys (opt-in participates in the global rules).
- sticky-only affirmative honors `AutoApprovePolicy.AllowSticky` (false → skip; true
  → press sticky index), consistent with Stage A.
- unrecognized pane and gate-off (`Enabled:false`, `s.AutoApprove=false`) → zero
  SendKeys (unchanged).

### `internal/config/config_test.go`

- **drift-guard** still passes: top-level yaml tags == schema keys, with `auto_approve`
  a single key and no `auto_approve_allow_sticky` entry.
- **Load legacy**: a file with flat `auto_approve: true` loads to
  `cfg.AutoApprove.Enabled == true` (via `UnmarshalYAML`), other keys intact, no
  parse-error fallback.
- **Load nested**: a file with a full `auto_approve:` block round-trips into the
  struct (enabled, allow_sticky, rules.allow/deny).
- **Reconcile migration**: an existing file with `auto_approve: true` and
  `auto_approve_allow_sticky: true` is rewritten so `auto_approve` is a mapping
  (`enabled: true`, `allow_sticky: true`, empty `rules`) and the flat
  `auto_approve_allow_sticky` key is gone; other keys, values, and comments are
  preserved. A second `Reconcile` on the migrated file is a no-op for that key.
- **Reconcile absent**: a brand-new file generates the nested block from defaults.

## Implementation order & verification

1. `internal/approval/policy.go` (+ tests) — self-contained; nothing else depends on
   it yet.
2. `internal/config` (struct/defaults/schema/migration + tests) — drift-guard +
   migration are the safety nets.
3. `internal/poller` (field + `Decide` call + tests) — depends on 1.
4. `internal/cli/daemon.go` (one line) — depends on 2 + 3.

Verify:

```
go test ./internal/approval/... ./internal/poller/... ./internal/config/...
go build ./...
go vet ./...
```

Given the surface area (a new engine + a real config migration + a poller rewrite),
run Stage B as its own short **pipeline** branching off the post-Stage-A tree:
`policy-engine → config-migrate → poller-wire → verify` (one shared worktree, the
same single-worktree technique used for Stage A). Do **not** start it until Stage A
is merged — Steps 2 and 3 edit the exact lines Stage A introduced.

## Risks / notes

- **Behavior change — fail-safe over fail-open.** After Stage B, `enabled: true` with
  an empty `allow` list approves **nothing** (previously "approve everything
  recognized"). This is the intended selective-by-default posture; it must be called
  out in the changelog and the config hint, and is the single biggest user-visible
  change in the whole feature.
- **Migration is the highest-risk change.** A bug in `migrateAutoApprove` can corrupt
  a user's `config.yaml`. Mitigations: the `UnmarshalYAML` shim means even an
  un-migrated or half-migrated file still *loads* correctly, so a Reconcile bug
  degrades to "file looks ugly," not "daemon won't start"; cover migration with the
  Reconcile tests above before wiring; keep `writeFile`'s existing atomic-ish write.
- **Glob surprises.** Users will expect `src/**` and `git push*` to "just work."
  The substring fallback for metachar-free patterns plus `**`→`.*` covers the common
  cases; document the exact semantics in the config hint and grow from real reports.
- **Per-subkey comments** in the generated block are not produced by the flat schema.
  Acceptable for Stage B (one comprehensive head-comment on `auto_approve`); a small
  special-case in `renderFull` to annotate sub-keys is optional polish.
- **Destructive guard precedence is load-bearing** and unit-asserted at the poller
  level (the "destructive still wins" case). Do not let a refactor reorder `Decide`
  ahead of `IsDestructive`.
- **`approval` gains a yaml dependency.** Minor, but if keeping `approval` yaml-free
  is preferred, the alternative is to define the yaml-mapped shape in `config` and
  convert to a yaml-free `approval.Policy`; that trades one import for a conversion
  layer and is explicitly *not* the chosen approach here.
