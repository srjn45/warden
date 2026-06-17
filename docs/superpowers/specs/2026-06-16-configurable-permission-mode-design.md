# Configurable Default Permission Mode — Design Spec

**Date:** 2026-06-16  
**Status:** Ready for implementation

---

## Summary

Make agent permission modes configurable via environment variable and per-agent overrides. Users can set a global default permission mode (replacing the hardcoded `acceptEdits`), and override it per-agent at spawn time or runtime.

**Problem:** All agents currently run with `--permission-mode acceptEdits` (hardcoded), which makes them "too chatty" with permission prompts. Users want control over the default behavior.

**Solution:** Add `WARDEN_DEFAULT_PERMISSION_MODE` env var with validation, replace `Session.Supervised` bool with `Session.PermissionMode` string, add CLI flags and commands for spawn-time and runtime configuration.

---

## Requirements

### R1: Global Default Configuration
- `WARDEN_DEFAULT_PERMISSION_MODE` env var controls default for all new agents
- Valid modes: `acceptEdits`, `auto`, `bypassPermissions`, `default`, `dontAsk`, `plan`
- Default value: `auto` (balanced middle ground)
- Invalid values: Log warning, fallback to `auto`
- Legacy `AGENTCTL_DEFAULT_PERMISSION_MODE` fallback for backwards compatibility

### R2: Per-Agent Override at Spawn
- `warden start --permission-mode <mode> "<prompt>"` spawns with explicit mode
- `warden start --supervised "<prompt>"` remains as alias for `--permission-mode acceptEdits`
- No flag = use global default from config
- Override persists in session and is used on restore/resume

### R3: Runtime Permission Mode Changes
- `warden set-permission-mode <agent-id> <mode>` changes mode for existing agent
- Changes persist across restarts/resumes
- API: `PATCH /sessions/{id}/permission-mode` with `{"mode": "..."}`

### R4: Session Storage Migration
- Replace `Session.Supervised` (bool) with `Session.PermissionMode` (string)
- Empty string = use global default
- Non-empty string = explicit per-agent override
- Old sessions without `PermissionMode` field: treat as empty (graceful degradation)

### R5: Observability
- `warden ls` shows permission mode in output
- `warden status <id>` displays current permission mode
- JSON API includes `permission_mode` field

---

## Architecture

### Configuration Layer

**Config struct:**
```go
type Config struct {
    // ... existing fields ...
    DefaultPermissionMode string // from WARDEN_DEFAULT_PERMISSION_MODE
}
```

**Validation logic:**
```go
func defaultPermissionMode() string {
    val := env("DEFAULT_PERMISSION_MODE")
    if val == "" {
        return "auto" // default
    }
    
    validModes := []string{"acceptEdits", "auto", "bypassPermissions", "default", "dontAsk", "plan"}
    for _, mode := range validModes {
        if val == mode {
            return mode
        }
    }
    
    log.Printf("WARN: invalid WARDEN_DEFAULT_PERMISSION_MODE=%q, using 'auto'", val)
    return "auto"
}
```

**Load flow:**
1. Read `WARDEN_DEFAULT_PERMISSION_MODE` (fallback to `AGENTCTL_DEFAULT_PERMISSION_MODE`)
2. Validate against 6 allowed modes
3. If invalid or empty: default to `auto`, log warning if invalid
4. Store in `Config.DefaultPermissionMode`

---

### Data Model

**Session struct changes:**
```go
type Session struct {
    // ... existing fields ...
    
    // REMOVED:
    // Supervised bool `json:"supervised"` 
    
    // NEW:
    PermissionMode string `json:"permission_mode,omitempty"` // explicit mode override; empty = use global default
    
    // ... rest of fields ...
}
```

**Store interface addition:**
```go
type Store interface {
    // ... existing methods ...
    UpdatePermissionMode(ctx context.Context, id string, mode string) error
}
```

**Permission mode resolution:**
```go
// Pseudo-code for resolving effective mode
func resolvePermissionMode(session *Session, config Config) string {
    if session.PermissionMode != "" {
        return session.PermissionMode  // per-agent override
    }
    return config.DefaultPermissionMode  // global default
}
```

**Migration strategy:**
- Old sessions with `Supervised` field but no `PermissionMode`: treat `PermissionMode` as `""` (use global default)
- No explicit migration script — read logic tolerates missing field
- First update to session will persist new structure with `PermissionMode` field

---

### Lifecycle Changes

**Updated function signatures:**
```go
// Before: func permissionFlag(supervised bool) string
// After:  func permissionFlag(mode string) string
func permissionFlag(mode string) string {
    return "--permission-mode " + mode
}

// Before: func claudeBase(supervised bool) string
// After:  func claudeBase(mode string) string
func claudeBase(mode string) string {
    return "claude --model claude-sonnet-4-5 " + permissionFlag(mode)
}
```

**Spawn logic:**
```go
func (l *Lifecycle) Spawn(ctx context.Context, params SpawnParams) (*Session, error) {
    // Resolve mode: explicit param or global default
    mode := params.PermissionMode
    if mode == "" {
        mode = l.cfg.DefaultPermissionMode
    }
    
    // Build command with resolved mode
    cmd := claudeLaunch(sessionID, name, mode) + pipelineHint()
    
    // Store override in session (empty if using default)
    sess := &Session{
        PermissionMode: params.PermissionMode, // store override, not resolved value
        // ...
    }
}
```

**Resume/Restore logic:**
```go
func (l *Lifecycle) Restore(ctx context.Context, id string) error {
    sess, _ := l.store.Get(ctx, id)
    
    // Resolve mode from session or global default
    mode := sess.PermissionMode
    if mode == "" {
        mode = l.cfg.DefaultPermissionMode
    }
    
    cmd := claudeResume(sess.ClaudeSessionID, sess.ID, mode)
    // ...
}
```

**SpawnParams addition:**
```go
type SpawnParams struct {
    // ... existing fields ...
    PermissionMode string // explicit mode override; empty = use global default
}
```

---

### CLI Design

**Spawn command:**
```bash
# Explicit mode via new flag
warden start --permission-mode auto "build the feature"
warden start --permission-mode bypassPermissions "run tests in isolation"

# Legacy --supervised flag (alias for --permission-mode acceptEdits)
warden start --supervised "review this PR"

# No flag: uses WARDEN_DEFAULT_PERMISSION_MODE (or 'auto' if not set)
warden start "implement the API"
```

**New set-permission-mode command:**
```bash
warden set-permission-mode <agent-id> <mode>

# Examples:
warden set-permission-mode abc123 bypassPermissions
warden set-permission-mode abc123 acceptEdits
warden set-permission-mode abc123 auto
```

**Command implementation:**
```go
func newSetPermissionModeCmd() *cobra.Command {
    return &cobra.Command{
        Use:   "set-permission-mode <agent-id> <mode>",
        Short: "Set the permission mode for an agent",
        Long: `Set the permission mode for a specific agent.
        
Valid modes: acceptEdits, auto, bypassPermissions, default, dontAsk, plan

The mode persists across restarts and resumes. Use an empty string to
reset to the global default from WARDEN_DEFAULT_PERMISSION_MODE.`,
        Args: cobra.ExactArgs(2),
        RunE: func(cmd *cobra.Command, args []string) error {
            id := args[0]
            mode := args[1]
            
            // Validate mode
            validModes := []string{"acceptEdits", "auto", "bypassPermissions", "default", "dontAsk", "plan"}
            valid := false
            for _, m := range validModes {
                if mode == m {
                    valid = true
                    break
                }
            }
            if !valid {
                return fmt.Errorf("invalid mode %q; must be one of: %s", mode, strings.Join(validModes, ", "))
            }
            
            c := clientFor(cmd)
            if err := c.SetPermissionMode(cmd.Context(), id, mode); err != nil {
                return err
            }
            
            fmt.Fprintf(cmd.OutOrStdout(), "permission mode set to %q for %s\n", mode, id)
            return nil
        },
    }
}
```

**List output enhancement:**
```
$ warden ls
ID       TYPE         STATUS   PERM_MODE        AGE   SUBJECT
abc123   development  working  bypassPermissions 2h   implementing auth
def456   analysis     idle     auto             30m  analyzing logs
ghi789   pr-review    waiting  acceptEdits      5m   reviewing PR #42
```

**Status output enhancement:**
```
$ warden status abc123
Agent: abc123
Type: development
Status: working
Permission Mode: bypassPermissions
...
```

---

### API Design

**New endpoint:**
```
PATCH /sessions/{id}/permission-mode
```

**Request body:**
```json
{
  "mode": "auto"
}
```

**Response:**
```json
// 200 OK
{
  "permission_mode": "auto"
}

// 404 Not Found
{
  "error": "session not found"
}

// 400 Bad Request (invalid mode)
{
  "error": "invalid mode \"foo\"; must be one of: acceptEdits, auto, bypassPermissions, default, dontAsk, plan"
}
```

**Handler implementation:**
```go
type SetPermissionModeRequest struct {
    Mode string `json:"mode"`
}

func (s *Server) handleSetPermissionMode(w http.ResponseWriter, r *http.Request) {
    id := chi.URLParam(r, "id")
    var req SetPermissionModeRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        writeErr(w, http.StatusBadRequest, "bad json")
        return
    }
    
    // Validate mode
    if !isValidPermissionMode(req.Mode) {
        writeErr(w, http.StatusBadRequest, fmt.Sprintf("invalid mode %q", req.Mode))
        return
    }
    
    if err := s.store.UpdatePermissionMode(r.Context(), id, req.Mode); err != nil {
        if errors.Is(err, store.ErrNotFound) {
            writeErr(w, http.StatusNotFound, "session not found")
            return
        }
        writeErr(w, http.StatusInternalServerError, err.Error())
        return
    }
    
    s.notify()
    writeJSON(w, http.StatusOK, map[string]string{"permission_mode": req.Mode})
}
```

**GET /sessions/{id} enhancement:**
Add `permission_mode` field to session JSON response.

**GET /sessions enhancement:**
Add `permission_mode` field to each session in list response.

---

## Migration Path

### Session Files

**Old format:**
```json
{
  "id": "abc123",
  "supervised": true,
  "status": "working",
  ...
}
```

**New format:**
```json
{
  "id": "abc123",
  "permission_mode": "acceptEdits",
  "status": "working",
  ...
}
```

**Migration strategy:**
1. **No explicit migration script** — graceful degradation
2. Old sessions without `permission_mode` field: treat as `""` (use global default)
3. Ignore `supervised` field if present (for read compatibility)
4. First update to session writes new format with `permission_mode`
5. Users can manually set mode via `warden set-permission-mode <id> <mode>` if desired

### Backwards Compatibility

**CLI:**
- `--supervised` flag continues to work as alias for `--permission-mode acceptEdits`
- No breaking changes to existing scripts/workflows

**Environment variables:**
- Legacy `AGENTCTL_DEFAULT_PERMISSION_MODE` still works (WARDEN_ takes precedence)

**Session JSON:**
- Old sessions without `permission_mode` use global default (no error)
- Code never writes `supervised` field (write-only new format)

---

## Testing Strategy

### Unit Tests

**Config validation:**
- Valid modes → accepted
- Invalid mode → warning logged, defaults to `auto`
- Empty mode → defaults to `auto`
- Legacy env var → works, WARDEN_ takes precedence

**Session store:**
- `UpdatePermissionMode()` persists to JSON
- Missing `permission_mode` field → read as `""`
- `permission_mode=""` → uses global default

**Lifecycle:**
- `permissionFlag()` returns correct flag for each mode
- Spawn with explicit mode → stored in session
- Spawn with no mode → uses global default, session has `permission_mode=""`
- Resume → uses session's mode or global default

### Integration Tests

**Spawn scenarios:**
```bash
# Test 1: Spawn with explicit mode
WARDEN_DEFAULT_PERMISSION_MODE=auto warden start --permission-mode bypassPermissions "test"
# Verify: agent runs with bypassPermissions

# Test 2: Spawn with --supervised
WARDEN_DEFAULT_PERMISSION_MODE=auto warden start --supervised "test"
# Verify: agent runs with acceptEdits

# Test 3: Spawn with no flag
WARDEN_DEFAULT_PERMISSION_MODE=bypassPermissions warden start "test"
# Verify: agent runs with bypassPermissions

# Test 4: Invalid global default
WARDEN_DEFAULT_PERMISSION_MODE=invalid warden start "test"
# Verify: warning logged, agent runs with auto
```

**Runtime mode changes:**
```bash
# Test 5: Set mode for running agent
warden set-permission-mode <id> acceptEdits
# Verify: session JSON updated, next resume uses new mode

# Test 6: Invalid mode
warden set-permission-mode <id> invalid
# Verify: error, no change to session
```

**Migration:**
```bash
# Test 7: Old session without permission_mode
# Setup: Create old-format session JSON with supervised=true
warden restore <id>
# Verify: agent resumes with global default mode

# Test 8: Daemon restart
# Setup: Set permission_mode for agent, restart daemon
warden restore <id>
# Verify: agent resumes with persisted mode
```

---

## Implementation Checklist

### Phase 1: Config and Data Model
- [ ] Add `DefaultPermissionMode` field to `Config` struct
- [ ] Implement `defaultPermissionMode()` validation helper
- [ ] Wire into `config.Load()`
- [ ] Add unit tests for config validation
- [ ] Replace `Session.Supervised` with `Session.PermissionMode` in types
- [ ] Add `UpdatePermissionMode()` to Store interface
- [ ] Implement `UpdatePermissionMode()` in FileStore
- [ ] Add unit tests for session persistence

### Phase 2: Lifecycle Integration
- [ ] Update `permissionFlag(mode string)` signature
- [ ] Update `claudeBase(mode string)` signature
- [ ] Update `claudeLaunch()` to accept mode parameter
- [ ] Update `claudeResume()` to accept mode parameter
- [ ] Add `PermissionMode` field to SpawnParams
- [ ] Update `Spawn()` to resolve and use permission mode
- [ ] Update `Restore()` to resolve and use permission mode
- [ ] Thread mode through SpawnJob (pipelines)
- [ ] Add unit tests for lifecycle functions

### Phase 3: CLI Implementation
- [ ] Add `--permission-mode` flag to start command
- [ ] Keep `--supervised` as alias for `--permission-mode acceptEdits`
- [ ] Create `set-permission-mode` command
- [ ] Add `SetPermissionMode()` to CLI client
- [ ] Update `warden ls` to show permission mode column
- [ ] Update `warden status` to show permission mode
- [ ] Add CLI integration tests

### Phase 4: API and Daemon
- [ ] Add `PATCH /sessions/{id}/permission-mode` route
- [ ] Implement `handleSetPermissionMode()` handler
- [ ] Add permission mode validation helper
- [ ] Update GET /sessions response to include `permission_mode`
- [ ] Update GET /sessions/{id} response to include `permission_mode`
- [ ] Add API integration tests

### Phase 5: Testing and Documentation
- [ ] Run full test suite
- [ ] Manual testing of all spawn scenarios
- [ ] Manual testing of runtime mode changes
- [ ] Test migration from old sessions
- [ ] Update FEATURES.md with permission mode documentation
- [ ] Update README with WARDEN_DEFAULT_PERMISSION_MODE
- [ ] Add examples to documentation

---

## Documentation

### FEATURES.md Addition

```markdown
## Permission Modes

Control how agents handle tool permissions via configurable permission modes.

**Global Default:**
- `WARDEN_DEFAULT_PERMISSION_MODE` env var (default: `auto`)
- Valid modes: `acceptEdits`, `auto`, `bypassPermissions`, `default`, `dontAsk`, `plan`

**Per-Agent Override:**
- Spawn: `warden start --permission-mode <mode> "<prompt>"`
- Runtime: `warden set-permission-mode <agent-id> <mode>`
- Legacy: `warden start --supervised "<prompt>"` (alias for `--permission-mode acceptEdits`)

**Mode Descriptions:**
- `acceptEdits`: Auto-approve file edits, prompt for other tools (balanced, chatty)
- `auto`: Claude's default permission behavior (recommended)
- `bypassPermissions`: Skip all permission prompts (autonomous, quiet)
- `default`: Claude's baseline permission handling
- `dontAsk`: Minimal prompting
- `plan`: Plan mode permission handling

**Examples:**
```bash
# Set global default to autonomous
export WARDEN_DEFAULT_PERMISSION_MODE=bypassPermissions
warden daemon

# Spawn with explicit mode
warden start --permission-mode acceptEdits "review this code"

# Change mode for existing agent
warden set-permission-mode abc123 auto

# View current mode
warden status abc123  # shows "Permission Mode: auto"
warden ls             # shows mode in PERM_MODE column
```
```

---

## Edge Cases and Considerations

### Empty vs. Explicit Default
- Session with `permission_mode=""` → uses global default (changes if env var changes)
- Session with `permission_mode="auto"` → locked to `auto` (ignores env var changes)
- This distinction allows: "use whatever default is configured" vs. "explicitly use auto"

### Invalid Mode Handling
- **At config load time:** Invalid `WARDEN_DEFAULT_PERMISSION_MODE` → warn, use `auto`
- **At spawn time:** Invalid `--permission-mode` → error, refuse to spawn
- **At runtime change:** Invalid mode in `set-permission-mode` → error, no change

### Mode Persistence Across Restore
- Agent spawned with `--permission-mode bypassPermissions`
- Daemon restarted with `WARDEN_DEFAULT_PERMISSION_MODE=acceptEdits`
- Agent restored → still uses `bypassPermissions` (session override wins)

### Model Parameter Interaction
- Permission mode is orthogonal to model selection
- `--model opus --permission-mode auto` works fine
- Both parameters persist independently in session

---

## Alternatives Considered

### Keep Supervised Bool
**Rejected:** Doesn't support all 6 permission modes, only binary supervised/unsupervised.

### Model-Specific Defaults
**Deferred:** Adds complexity without clear use case. Can add later if needed.

### Config File Instead of Env Var
**Rejected:** Inconsistent with existing warden config pattern (all env vars).

### Allow Custom Permission Strings
**Rejected:** Should only accept Claude-supported modes. Validation prevents typos.

---

## Summary

This design enables users to configure the default permission mode via `WARDEN_DEFAULT_PERMISSION_MODE`, with per-agent overrides at spawn time (`--permission-mode`) or runtime (`set-permission-mode`). The implementation replaces the boolean `Supervised` field with a string `PermissionMode` field, gracefully handling migration from old session files. All 6 Claude permission modes are supported with validation.
