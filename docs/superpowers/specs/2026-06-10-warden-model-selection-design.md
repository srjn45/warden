# Warden Model Selection — Design Spec

**Date:** 2026-06-10  
**Updated:** 2026-06-14  
**Status:** Ready for implementation (Phase 1: Manual + Simple Default)

---

## Summary

Allow warden to specify which Claude model each agent uses via `--model` flag, with a configurable default via environment variable. The core principle: model selection should be transparent, configurable, and never surprising.

**Phase 1 (This implementation):** Manual override + simple default  
**Future phases:** Type-based defaults, prompt classification

---

## Phase 1 Implementation: Manual + Simple Default

### Store Change

Add `Model string` to `store.Session`:

```go
// internal/store/types.go
type Session struct {
    // ... existing fields ...
    Model string `json:"model,omitempty"` // claude model: "opus", "sonnet", "haiku", or full ID; empty = default
}
```

**Why `omitempty`:** Backward compatibility - existing sessions without model deserialize correctly.

**Model resolution order:**
1. Explicit `--model` flag (if provided)
2. `WARDEN_MODEL_DEFAULT` env var (if set)
3. Hardcoded default: `claude-sonnet-4-5`

Persisted so `warden ls` and digests can report what model an agent ran on. Restored sessions reuse the stored model.

---

## Model Resolution Logic

### Default Resolution

```go
// internal/lifecycle/models.go (new file)
package lifecycle

// resolveDefaultModel returns the model to use when none is explicitly provided.
// Checks WARDEN_MODEL_DEFAULT env var, falls back to claude-sonnet-4-5.
func resolveDefaultModel() string {
    if envModel := os.Getenv("WARDEN_MODEL_DEFAULT"); envModel != "" {
        return ResolveModel(envModel) // support aliases in env var too
    }
    return "claude-sonnet-4-5"
}

// modelOrDefault returns the resolved model ID to use: the provided model
// (with aliases expanded), or the default if model is empty.
func modelOrDefault(model string) string {
    if model != "" {
        return ResolveModel(model)
    }
    return resolveDefaultModel()
}
```

**Environment variable:**
```bash
# Set user preference for default model
export WARDEN_MODEL_DEFAULT=opus     # use opus for all agents by default
export WARDEN_MODEL_DEFAULT=haiku    # use haiku for all agents by default
```

---

## Future Enhancement: Auto-Selection (Not Implemented in Phase 1)

### Step 1 — Configurable default per agent type (fast, no LLM call)

### Step 1 — Configurable default per agent type (fast, no LLM call)

A map of agent type → default model ID, configured via env vars:

```
WARDEN_MODEL_SUPERVISED=claude-opus-4-8      # supervised worktree agents (risky, complex)
WARDEN_MODEL_PIPELINE=claude-haiku-4-5       # pipeline jobs (parallel, disposable)
WARDEN_MODEL_FREEFORM=claude-sonnet-4-6      # default free-form agents
WARDEN_MODEL_INTERACTIVE=claude-sonnet-4-6   # interactive (no prompt) agents
```

These are the defaults. Operators can tune them (e.g., point pipeline jobs at Sonnet if quality matters more than cost for their workload). New agent types added in the future get a default without touching lifecycle code.

### Step 2 — Prompt classification (optional, ~1-2s latency)

Applied only when Step 1 resolves to the free-form default (Sonnet) and `WARDEN_MODEL_CLASSIFY=1` is set (off by default — opt-in since it adds latency and burns tokens).

The daemon runs:
```
claude -p "Classify this task complexity: simple|moderate|complex. One word only. Task: <first 500 chars of prompt>"
```

Maps to: `simple → haiku`, `moderate → sonnet`, `complex → opus`.

Step 2 is skipped entirely when:
- Agent type already resolved to a non-Sonnet model in Step 1
- `WARDEN_MODEL_CLASSIFY=0` (or not set)
- Prompt is empty (interactive agent)

### Manual Override (always wins)

Manual selection takes precedence over both steps above.

| Surface | How |
|---|---|
| CLI | `warden spawn --model opus` (or `haiku`, `sonnet`, full model ID) |
| Web | Dropdown in `NewAgentModal` (shows friendly names + note on cost/capability) |
| MCP | `model` param in spawn tool |
| Pipeline spec | `model` field per job in pipeline YAML/JSON |

The resolved model is logged in the spawn event for auditability.

---

## Short Model Name Aliases (Phase 1)

Daemon maps short aliases to full versioned model IDs at spawn time:

```go
// internal/lifecycle/models.go
var modelAliases = map[string]string{
    "opus":   "claude-opus-4-8",
    "sonnet": "claude-sonnet-4-6", 
    "haiku":  "claude-haiku-4-5",
    "fable":  "claude-fable-5",
}

// ResolveModel maps short alias to full model ID, or returns input unchanged
// if it's already a full ID or unknown (let claude CLI validate it).
func ResolveModel(input string) string {
    if input == "" {
        return ""
    }
    if full, ok := modelAliases[input]; ok {
        return full
    }
    return input // assume it's already a full model ID
}
```

| Alias | Resolves to |
|---|---|
| `haiku` | `claude-haiku-4-5` |
| `sonnet` | `claude-sonnet-4-6` |
| `opus` | `claude-opus-4-8` |
| `fable` | `claude-fable-5` |

Full model IDs are also accepted directly (for pinning to a specific version).

**No validation:** The `claude` CLI is authoritative for valid models. We translate common aliases and pass through everything else. Invalid models will error when `claude` is invoked.

The alias→ID mapping is updated with Claude releases — one place in the daemon, not scattered across CLI/MCP/web.

---

## Display (Phase 1)

### CLI `warden ls`

Add MODEL column to output:

```
NAME    ID              TYPE         MODEL   STATUS   CONTEXT  AGE    DIR      SUBJECT
arch    agent-d4ac3ed9  analysis     opus    working  45k      5m     warden   Review the design spec...
perf    agent-a8c321c0  analysis     opus    working  38k      5m     warden   Review the design spec...
```

Implementation:

```go
// internal/cli/sessions.go
func newLsCmd() *cobra.Command {
    // Update header
    fmt.Fprintln(tw, "NAME\tID\tTYPE\tMODEL\tSTATUS\tCONTEXT\tAGE\tDIR\tSUBJECT")
    
    for _, s := range sessions {
        modelBadge := modelCell(s.Model)
        fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
            name, s.ID, typeOrPending(s.Type), modelBadge, s.Status,
            contextCell(...), age(s.UpdatedAt), dirName(s.Workdir), s.Subject)
    }
}

// modelCell displays short alias if model matches known ID, otherwise full ID
func modelCell(model string) string {
    if model == "" {
        return "sonnet" // default
    }
    // Show short alias if known
    for alias, fullID := range lifecycle.modelAliases {
        if model == fullID {
            return alias
        }
    }
    return model // show full ID if custom
}
```

### CLI `warden status`

Add model field to detailed output:

```
id:         agent-123
name:       arch-review
type:       analysis
model:      claude-opus-4-8
status:     working
...
```

### Future Display Enhancements (Not Phase 1)

- **TUI list row:** model badge after status (e.g., `[opus]`)
- **TUI agent detail overlay (`i`):** model field
- **Web agent tab header:** model badge alongside context size badge
- **Digest:** "ran on claude-opus-4-8" in summary

---

## What Gets Passed to Claude (Phase 1)

The `--model <id>` flag is always passed to `claudeBase()` / `claudeLaunch()` / `claudeResume()` in lifecycle.

### Lifecycle Changes

```go
// internal/lifecycle/lifecycle.go

// Before:
func claudeBase(supervised bool) string {
    return "claude --model claude-sonnet-4-5 " + permissionFlag(supervised)
}

// After:
func claudeBase(model string, supervised bool) string {
    modelID := modelOrDefault(model)
    return "claude --model " + modelID + " " + permissionFlag(supervised)
}

// claudeLaunch builds the claude invocation for a spawned agent
func claudeLaunch(sessionID, name string, model string, supervised bool) string {
    return claudeBase(model, supervised) + " --session-id " + sessionID + " --name " + shellQuoteArg(name)
}

// claudeResume builds the invocation for resuming an agent
func claudeResume(sessionID, name string, model string, supervised bool) string {
    return claudeBase(model, supervised) + " --resume " + sessionID + " --name " + shellQuoteArg(name)
}
```

**All callers updated:** Spawn, SpawnJob, Restore must pass model parameter.

---

## CLI Interface (Phase 1)

### Start Command

Add `--model` flag:

```go
// internal/cli/lifecycle.go
func newStartCmd() *cobra.Command {
    cmd := &cobra.Command{
        Use:   "start [TICKET|\"<prompt>\"] [--type <TYPE>] [--dir <PATH>]",
        Short: "Spawn an agent...",
        RunE: func(cmd *cobra.Command, args []string) error {
            // ... existing code ...
            
            model, _ := cmd.Flags().GetString("model")
            
            s, err := clientFor(cmd).Spawn(cmd.Context(), client.SpawnParams{
                // ... existing fields ...
                Model: model,
            })
            // ... rest unchanged ...
        },
    }
    
    // ... existing flags ...
    cmd.Flags().String("model", "", "claude model: opus, sonnet, haiku, fable, or full model ID (default: sonnet-4.5 or WARDEN_MODEL_DEFAULT)")
    
    return cmd
}
```

**Usage examples:**

```bash
# Explicit model
warden start "Review design spec" --model opus

# Use env var default
export WARDEN_MODEL_DEFAULT=opus
warden start "Review design spec"  # uses opus

# Fallback to hardcoded default
warden start "Review design spec"  # uses sonnet-4.5
```

---

## Client Layer (Phase 1)

### SpawnParams

Add Model field:

```go
// internal/client/client.go
type SpawnParams struct {
    Type        string
    Ticket      string
    Name        string
    Repo        string
    Branch      string
    PR          string
    Worktree    bool
    Prompt      string
    Cwd         string
    Supervised  bool
    AutoRestart bool
    Force       bool
    Model       string  // NEW: claude model
}

func (c *Client) Spawn(ctx context.Context, p SpawnParams) (*store.Session, error) {
    body := map[string]any{
        "type": p.Type, "ticket": p.Ticket, "name": p.Name, "repo": p.Repo,
        "branch": p.Branch, "pr": p.PR, "worktree": p.Worktree,
        "prompt": p.Prompt, "cwd": p.Cwd, "supervised": p.Supervised,
        "auto_restart": p.AutoRestart, "force": p.Force,
        "model": p.Model,  // NEW
    }
    // ... rest unchanged ...
}
```

**No daemon changes needed:** The spawn endpoint already unmarshals arbitrary JSON fields.

---

## MCP Tool (Phase 1)

### spawn_agent Tool

Add model parameter:

```go
// internal/mcp/server.go
type spawnArgs struct {
    Type       string `json:"type,omitempty" jsonschema:"..."`
    Ticket     string `json:"ticket,omitempty" jsonschema:"..."`
    // ... existing fields ...
    Model      string `json:"model,omitempty" jsonschema:"claude model: opus, sonnet, haiku, fable, or full model ID; defaults to sonnet-4.5 or WARDEN_MODEL_DEFAULT"`
}

// In spawn handler:
func (s *Server) handleSpawn(args spawnArgs) (*store.Session, error) {
    sess, err := s.cl.Spawn(ctx, client.SpawnParams{
        // ... existing fields ...
        Model: args.Model,
    })
    // ... rest unchanged ...
}
```

**Usage from agents:**

```python
# Spawn agent with specific model
spawn_agent(prompt="Review code", model="opus")

# Use default
spawn_agent(prompt="Review code")  # uses WARDEN_MODEL_DEFAULT or sonnet-4.5
```

---

## Testing Strategy (Phase 1)

### Unit Tests

1. **ResolveModel()** - alias mapping
   ```go
   func TestResolveModel(t *testing.T) {
       assert.Equal(t, "claude-opus-4-8", ResolveModel("opus"))
       assert.Equal(t, "claude-sonnet-4-6", ResolveModel("sonnet"))
       assert.Equal(t, "claude-custom-1", ResolveModel("claude-custom-1")) // passthrough
       assert.Equal(t, "", ResolveModel(""))
   }
   ```

2. **modelOrDefault()** - env var fallback
   ```go
   func TestModelOrDefault(t *testing.T) {
       os.Setenv("WARDEN_MODEL_DEFAULT", "haiku")
       assert.Equal(t, "claude-haiku-4-5", modelOrDefault(""))
       assert.Equal(t, "claude-opus-4-8", modelOrDefault("opus"))
       os.Unsetenv("WARDEN_MODEL_DEFAULT")
       assert.Equal(t, "claude-sonnet-4-5", modelOrDefault(""))
   }
   ```

3. **claudeBase()** - command construction
   ```go
   func TestClaudeBase(t *testing.T) {
       cmd := claudeBase("opus", false)
       assert.Contains(t, cmd, "--model claude-opus-4-8")
       
       cmd = claudeBase("", false)
       assert.Contains(t, cmd, "--model claude-sonnet-4-5")
   }
   ```

### Integration Tests

1. Spawn with `--model opus`, verify Session.Model = "claude-opus-4-8"
2. Spawn without `--model`, verify falls back to default
3. Set `WARDEN_MODEL_DEFAULT=haiku`, spawn without flag, verify uses haiku

### Manual Testing

```bash
# Test explicit flag
warden start "test" --model opus
warden ls  # should show "opus" in MODEL column

# Test env var default
export WARDEN_MODEL_DEFAULT=haiku
warden start "test"
warden ls  # should show "haiku"

# Test hardcoded default
unset WARDEN_MODEL_DEFAULT
warden start "test"
warden ls  # should show "sonnet"

# Test MCP tool
warden start "test MCP" --model fable
warden status test-mcp  # should show model: claude-fable-5
```

---

## Implementation Checklist (Phase 1)

- [ ] Add `Model string` field to `store.Session`
- [ ] Create `internal/lifecycle/models.go` with aliases and resolution functions
- [ ] Update `claudeBase()`, `claudeLaunch()`, `claudeResume()` to accept model parameter
- [ ] Update all lifecycle callers (Spawn, SpawnJob, Restore) to pass model
- [ ] Add `--model` flag to CLI `start` command
- [ ] Add `Model` field to `client.SpawnParams`
- [ ] Update client Spawn to send model in request body
- [ ] Add `model` parameter to MCP `spawnArgs`
- [ ] Add MODEL column to `warden ls` output
- [ ] Add `modelCell()` helper to display short aliases
- [ ] Add model field to `warden status` output
- [ ] Write unit tests for model resolution
- [ ] Write integration tests for spawn with/without model
- [ ] Update README with `--model` flag documentation
- [ ] Update FEATURES.md with model selection feature

**Estimated time:** 3-4 hours

---

## Future Enhancements (Post-Phase 1)

### Phase 2: Type-Based Defaults

Add per-agent-type env vars:
```
WARDEN_MODEL_DEVELOPMENT=opus     # development agents use opus
WARDEN_MODEL_PIPELINE=haiku       # pipeline jobs use haiku
WARDEN_MODEL_ANALYSIS=sonnet      # analysis agents use sonnet
```

### Phase 3: Prompt Classification

Optional complexity-based selection via `WARDEN_MODEL_CLASSIFY=1`.

### Phase 4: Web UI

Model dropdown in spawn modal.

---

## Open Questions (Future Phases)

1. Should pipeline jobs inherit the pipeline's model or always use `WARDEN_MODEL_PIPELINE`? Probably per-job override in the pipeline spec with pipeline-level default.
2. Classification prompt: is a single-word response reliable enough, or do we need structured output (`claude -p` with `--output-format json`)?
3. Cost display: show estimated cost bracket (cheap/mid/expensive) in the model selector as a UX hint?
4. Should restore always reuse the stored model, or offer to re-classify on restore?
