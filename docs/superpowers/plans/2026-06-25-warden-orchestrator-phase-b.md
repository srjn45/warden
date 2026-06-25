# Warden Orchestrator — Phase B: Loop + Tool Registry + Confirm Gate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the first usable orchestrator: a `wd orch` REPL that turns a natural-language line into **confirmed** warden tool calls. This is the milestone where the design's three load-bearing pieces land — the **tool registry** (warden's daemon surface as model-callable functions, split read-only vs. mutating), the **confirm gate** (every mutation rendered before it runs), and the **loop** (the `Chatter` turn cycle with bounded recovery and tier routing). Fully usable standalone in any terminal; the cockpit/`!`-shell wiring is Phase C, monitoring verbs are Phase D.

**Design spec:** `docs/superpowers/specs/2026-06-25-warden-orchestrator-design.md` (Phase B + "Hardware-aware model selection & capability tiers")

**Depends on:** Phase A (`internal/llm.Chatter`, `Message`/`ToolSchema`/`ToolCall`/`Reply`, `decodeArgs`, the `fakeChatter` test double). Brain spec Phase 0b/0c (the `commit`/`push`/`sync`/`check` lifecycle the registry calls) and Phase 1a (the `*Ollama` provider it type-asserts to `Chatter`) — all shipped.

**Architecture:** A new `internal/orchestrator` package, a second front-end onto `client.Client` (the same client the MCP server wraps — **no new business logic**), plus a `wd orch` cobra subcommand. The package is built on a narrow `Daemon` interface that `*client.Client` already satisfies, so the loop, registry, and gate are all testable against a fake without a live daemon.

```
  internal/orchestrator/
    daemon.go     — the narrow Daemon interface (subset of *client.Client) + the read/mutate split
    registry.go   — []Tool: each warden verb as {schema, sideEffect, invoke}; ToolSchemas() + Dispatch()
    gate.go       — Gate: render proposed mutating call(s), read approve/edit/reject from an io seam
    tier.go       — complexity pre-classify (T0/T1/T2) → plan-local | escalate-to-Claude | degrade
    session.go    — the Chat turn loop: assemble context, auto-run reads, gate mutations, feed results back, recover
  internal/cli/orch.go   — `wd orch` (alias `orchestrator`): build client + Chatter from config, run the REPL
  internal/config        — orchestrator / local_llm_escalate / local_llm_tier keys
```

**Tech Stack:** Go 1.26+, stdlib + cobra + testify (as the repo uses). Reuses `internal/llm` (Phase A), `internal/client`, `internal/config`. No new third-party deps.

**Scope guard:** **No `!` shell passthrough, no PTY, no cockpit/TUI changes, no embedded shell** — those are Phase C. **No fleet-summarization NL verbs** — that is Phase D (this phase exposes the read-only tools to the model, but does not add the "what's stuck"-style summarization pass). If a task here reaches into the TUI or a PTY, stop and re-scope to C.

---

## File Structure

### New Files
- `internal/orchestrator/daemon.go` — `Daemon` interface + compile-time `var _ Daemon = (*client.Client)(nil)`
- `internal/orchestrator/registry.go` + `registry_test.go` — the tool table, `ToolSchemas()`, `Dispatch()`
- `internal/orchestrator/gate.go` + `gate_test.go` — the confirm gate
- `internal/orchestrator/tier.go` + `tier_test.go` — capability-tier routing
- `internal/orchestrator/session.go` + `session_test.go` — the loop
- `internal/cli/orch.go` + `orch_test.go` — the `wd orch` command

### Modified Files
- `internal/cli/root.go` — register `newOrchCmd()` (one `AddCommand` line, alongside `newTUICmd()` at root.go:45)
- `internal/config/config.go` — three new keys (struct fields, defaults, descriptions, accessors)

---

## Task 1: The `Daemon` seam + the tool registry

**Files:** New `internal/orchestrator/daemon.go`, `internal/orchestrator/registry.go`, `internal/orchestrator/registry_test.go`

The registry is the safety backbone: it is the *complete* list of what the orchestrator can do, and the read/mutate flag on each entry is what the gate keys off. There is **deliberately no edit/write/bash/shell tool** — that absence is the structural enforcement of "conducts, never implements," and a test asserts it.

- [ ] **Step 1: Define the `Daemon` interface (`daemon.go`)**

A narrow interface holding only the `*client.Client` methods the registry calls, so the package tests against a fake. Reads and mutations both live here; the split is expressed per-tool in the registry, not by interface.

```go
package orchestrator

// Daemon is the subset of *client.Client the orchestrator drives. *client.Client
// satisfies it as-is — the orchestrator is a second front-end onto the same
// client the MCP server uses, never a reimplementation.
type Daemon interface {
    // reads
    List(ctx context.Context) ([]*store.Session, error)
    Get(ctx context.Context, id string) (*store.Session, error)
    Output(ctx context.Context, id string, lines int) (string, error)
    Approvals(ctx context.Context) (bool, []approval.View, error)
    MsgInbox(ctx context.Context, id string, unreadOnly bool) ([]client.Message, error)
    CtxGet(ctx context.Context, key string) (client.ContextEntry, error)
    CtxList(ctx context.Context, prefix string) ([]client.ContextEntry, error)
    PipelineList(ctx context.Context) ([]*pipeline.Pipeline, error)
    PipelineGet(ctx context.Context, id string) (*pipeline.Pipeline, error)
    CollabConflicts(ctx context.Context) ([]client.Conflict, error)
    // mutations
    Spawn(ctx context.Context, p client.SpawnParams) (*store.Session, error)
    Input(ctx context.Context, id, text string) error
    Terminate(ctx context.Context, id string) error
    Delete(ctx context.Context, id string, hard bool) error
    Restore(ctx context.Context, id string) error
    Approve(ctx context.Context, id string, option int, fingerprint string) error
    GitCommit(ctx context.Context, session, dir, message string) (lifecycle.CommitResult, error)
    GitPush(ctx context.Context, session, dir string) (lifecycle.PushResult, error)
    GitSync(ctx context.Context, session, dir, base string) (lifecycle.SyncResult, error)
    Check(ctx context.Context, session, dir, name string) (lifecycle.CheckResult, error)
    CtxSet(ctx context.Context, key, value, by string) (client.ContextEntry, error)
    MsgSend(ctx context.Context, to, from, body string) (client.Message, bool, error)
    PipelineCreate(ctx context.Context, specYAML string) (*pipeline.Pipeline, error)
    PipelineCancel(ctx context.Context, id string) error
}

var _ Daemon = (*client.Client)(nil)
```

(Start with this set; `adopt`/`ctx_cas`/`ctx_append`/`restore`/pipeline edit verbs can be added as the registry grows — keep the interface and the registry in lockstep.)

- [ ] **Step 2: Write registry tests first (`registry_test.go`)**

Drive the contract: every tool has a non-empty name/description and an object-typed parameter schema; the read/mutate flag matches the spec's split; **no mutating tool's name is one of `edit`/`write`/`bash`/`shell`/`exec`**; and `Dispatch` routes a named call to the right `Daemon` method with decoded args.

```go
func TestRegistry_SplitMatchesSpec(t *testing.T) {
    reg := NewRegistry()
    readOnly := map[string]bool{"list_agents": true, "get_agent": true, "get_agent_output": true,
        "get_collaboration_status": true, "read_inbox": true, "list_approvals": true,
        "ctx_get": true, "ctx_list": true, "pipeline_list": true, "pipeline_get": true}
    for _, tl := range reg.Tools() {
        require.NotEmpty(t, tl.Schema.Name)
        require.NotEmpty(t, tl.Schema.Description)
        require.Equal(t, "object", tl.Schema.Parameters["type"], tl.Schema.Name)
        require.Equal(t, readOnly[tl.Schema.Name], !tl.Mutating, "side-effect flag for %s", tl.Schema.Name)
    }
}

func TestRegistry_HasNoCodeEditingTool(t *testing.T) {
    for _, tl := range NewRegistry().Tools() {
        require.NotContains(t, []string{"edit", "write", "bash", "shell", "exec", "run"}, tl.Schema.Name,
            "the orchestrator must have no code-editing/shell tool — it conducts, never implements")
    }
}

func TestDispatch_RoutesToDaemon(t *testing.T) {
    fd := &fakeDaemon{}
    reg := NewRegistry()
    _, err := reg.Dispatch(context.Background(), fd, ToolCall{Name: "spawn_agent",
        Args: map[string]any{"type": "development", "prompt": "refactor auth"}})
    require.NoError(t, err)
    require.Equal(t, "development", fd.lastSpawn.Type)
    require.Equal(t, "refactor auth", fd.lastSpawn.Prompt)
}

func TestDispatch_UnknownToolErrors(t *testing.T) {
    _, err := NewRegistry().Dispatch(context.Background(), &fakeDaemon{}, ToolCall{Name: "nope"})
    require.ErrorContains(t, err, "unknown tool")
}
```

- [ ] **Step 3: Implement `registry.go`**

```go
// Tool is one warden verb the model may call.
type Tool struct {
    Schema   llm.ToolSchema
    Mutating bool // false ⇒ auto-execute (read); true ⇒ confirm gate
    invoke   func(ctx context.Context, d Daemon, args map[string]any) (string, error)
}

type Registry struct{ tools []Tool }

func NewRegistry() *Registry { /* the static table; one Tool per spec verb */ }
func (r *Registry) Tools() []Tool { return r.tools }
func (r *Registry) ToolSchemas() []llm.ToolSchema { /* map .Schema for the Chat call */ }
func (r *Registry) Lookup(name string) (Tool, bool) { /* ... */ }

// Dispatch validates name against the registry and runs the tool. The result
// string is what's fed back to the model as the tool result on the next turn.
func (r *Registry) Dispatch(ctx context.Context, d Daemon, c ToolCall) (string, error) {
    tl, ok := r.Lookup(c.Name)
    if !ok {
        return "", fmt.Errorf("unknown tool %q", c.Name)
    }
    return tl.invoke(ctx, d, c.Args)
}
```

Each `invoke` pulls typed args out of the `map[string]any` (with sane coercion — a model may send a number where a string is expected), calls the matching `Daemon` method, and renders the result compactly (reuse the MCP server's JSON shaping where it helps; the model reads these as plain text). Args are read defensively: a missing required arg returns an error string the loop feeds back so the model can retry — it never panics on a malformed map.

- [ ] **Step 4: Run → fail → implement → pass → commit**

```bash
cd internal/orchestrator && go test ./...
```

Commit: `feat(orchestrator): tool registry over the daemon client (read/mutate split, no code-editing tool)`.

---

## Task 2: The confirm gate

**Files:** New `internal/orchestrator/gate.go`, `internal/orchestrator/gate_test.go`

Every mutating call is rendered as the concrete action it will take and **confirmed before execution**. Batched plans confirm as one unit. This is non-negotiable and not config-gated — it is the reason a 7B model is safe in this seat.

- [ ] **Step 1: Tests first**

Gate I/O goes through an `io.Reader`/`io.Writer` seam so it's scriptable. Cover: a single call renders + `a`pprove returns Approve; `r`eject returns Reject and runs nothing; `e`dit yields the edited args; a batch renders all calls and confirms once; EOF/`\n` defaults to reject (safe default).

```go
func TestGate_ApproveRunsAll(t *testing.T) {
    g := NewGate(strings.NewReader("a\n"), io.Discard)
    d := g.Confirm([]ToolCall{{Name: "spawn_agent", Args: map[string]any{"type": "development"}}})
    require.Equal(t, Approve, d.Action)
}
func TestGate_RejectByDefaultOnEOF(t *testing.T) {
    g := NewGate(strings.NewReader(""), io.Discard)
    require.Equal(t, Reject, g.Confirm([]ToolCall{{Name: "commit"}}).Action)
}
func TestGate_RendersEveryCallInBatch(t *testing.T) {
    var out bytes.Buffer
    NewGate(strings.NewReader("r\n"), &out).Confirm([]ToolCall{
        {Name: "spawn_agent", Args: map[string]any{"prompt": "a"}},
        {Name: "spawn_agent", Args: map[string]any{"prompt": "b"}}})
    require.Equal(t, 2, strings.Count(out.String(), "spawn_agent"))
}
```

- [ ] **Step 2: Implement**

```go
type Action int
const ( Reject Action = iota; Approve; Edit )

type Decision struct {
    Action Action
    Calls  []ToolCall // for Edit: the operator-revised calls
}

type Gate struct { in *bufio.Scanner; out io.Writer }
func NewGate(r io.Reader, w io.Writer) *Gate { /* ... */ }

// Confirm renders the proposed mutating calls and blocks for [a]pprove /
// [e]dit / [r]eject. Reject is the default on EOF or an unrecognized key.
func (g *Gate) Confirm(calls []ToolCall) Decision { /* render + read one key */ }
```

Render format mirrors the spec example (`orchestrator wants to:` + one line per call + `[a]pprove [e]dit [r]eject`). `Edit` re-prompts per-arg; keep the edit UX minimal for the MVP (re-enter the JSON args for a call), and note in a comment that a richer field-by-field editor is a follow-up.

- [ ] **Step 3: Run → pass → commit**

Commit: `feat(orchestrator): confirm gate renders mutating calls before execution`.

---

## Task 3: The loop (`session.go`)

**Files:** New `internal/orchestrator/session.go`, `internal/orchestrator/session_test.go`

The turn cycle that ties Chatter + registry + gate together. Read-only calls auto-execute; mutating calls go through the gate; tool results feed back; the loop ends when the model yields prose or the turn budget is spent. Built against `fakeChatter` (Phase A) + `fakeDaemon` + a scripted gate, so the whole loop is tested with no live model or daemon.

- [ ] **Step 1: Tests first — the behaviors that matter**

  - **Read auto-executes, no gate:** a `Reply` with `list_agents` runs immediately, result feeds back, model's next prose is printed; the gate is never consulted.
  - **Mutation hits the gate; reject ⇒ nothing runs:** a `spawn_agent` reply with a rejecting gate leaves `fakeDaemon.spawnCalls == 0`.
  - **Mutation approved ⇒ runs, result feeds back.**
  - **Recovery — unknown tool:** model calls a tool not in the registry → loop feeds back "unknown tool" as the tool result and lets the model retry within the budget (assert it doesn't crash and the error text reaches the next Chat call's messages).
  - **Recovery — malformed args** already error at `decodeArgs`/Dispatch; assert the loop converts that to a fed-back tool result, not a fatal.
  - **Turn budget:** a model that calls tools forever stops after `maxTurns` with an honest "couldn't complete" message.
  - **Batched plan confirms once:** two `spawn_agent` calls in one `Reply` → one `Gate.Confirm` call.

```go
func TestSession_ReadOnlyAutoExecutes(t *testing.T) {
    chat := scriptChatter{ // turn 1: tool call; turn 2: prose
        {ToolCalls: []ToolCall{{Name: "list_agents"}}},
        {Text: "2 agents running."}}
    s := newTestSession(&chat, &fakeDaemon{sessions: twoSessions()}, alwaysReject())
    out := s.Handle(context.Background(), "what's running?")
    require.Contains(t, out, "2 agents running")
    require.Zero(t, s.gate.(*spyGate).confirmCalls, "reads never hit the gate")
}
```

- [ ] **Step 2: Implement the loop**

```go
const maxTurns = 6 // bounded so a confused model can't loop forever

type Session struct {
    chat  llm.Chatter
    daem  Daemon
    reg   *Registry
    gate  *Gate
    tier  *Router // Task 4
    msgs  []llm.Message // running conversation
    out   io.Writer
}

// Handle runs one operator line to completion (prose) or to the turn budget.
func (s *Session) Handle(ctx context.Context, line string) string {
    s.msgs = append(s.msgs, llm.Message{Role: llm.RoleUser, Content: line})
    s.refreshFleetSnapshot(ctx) // compact one-line-per-agent digest into the system context
    for turn := 0; turn < maxTurns; turn++ {
        reply, err := s.chat.Chat(ctx, s.msgs, s.reg.ToolSchemas())
        if err != nil { return s.surface(err) } // local model down ⇒ honest message
        s.msgs = append(s.msgs, assistantMsg(reply))
        if len(reply.ToolCalls) == 0 { return reply.Text } // model yielded prose
        s.runCalls(ctx, reply.ToolCalls) // reads now; mutations via gate; append results
    }
    return "couldn't complete that within the turn budget — try a smaller ask"
}
```

`runCalls` partitions the batch: read-only calls dispatch immediately; the mutating ones are gathered and sent to `Gate.Confirm` as a unit (approve→dispatch each, edit→dispatch edited, reject→feed back "operator rejected"). Each dispatch result (or error string) is appended as a `RoleTool` message so the model sees what happened.

System prompt: *"You are warden's orchestrator. You conduct agents and pipelines and run the git/check lifecycle through the provided tools. You never write or edit code — to do code work, spawn a Claude agent. Propose tool calls; the operator confirms every mutation."* Plus the compact fleet snapshot (the spec's open question: one line per agent, not full records — keep it under a few hundred tokens).

- [ ] **Step 3: Run → pass → commit**

Commit: `feat(orchestrator): NL→tool-call loop with auto-read, gated mutations, bounded recovery`.

---

## Task 4: Capability-tier routing

**Files:** New `internal/orchestrator/tier.go`, `internal/orchestrator/tier_test.go`

Before the expensive planning turn, a cheap T0 classify buckets the request's needed tier; if it exceeds the configured model's tier, escalate the *one planning step* to headless Claude or degrade honestly. This is where the spec's hardware/tier story becomes code. Execution stays token-free warden tool calls regardless of who drafted the plan.

- [ ] **Step 1: Tests first**

  - needed ≤ model → `PlanLocal` (no escalation, no Claude).
  - needed > model, escalate on → `Escalate` path is taken (fake escalator returns a plan; assert the loop would use it).
  - needed > model, escalate off (or Claude unavailable) → `Degrade` with an operator-facing message; **never** a silent local attempt.
  - model→tier table: `1.5b→T0`, `3b→T0/T1`, `7b→T2`, `14b+→T2` (assert `modelTier("qwen2.5-coder:3b") == T1`, etc.), plus `local_llm_tier` override wins.

```go
func TestRouter_BelowTierEscalatesWhenEnabled(t *testing.T) {
    r := NewRouter(modelTier("qwen2.5-coder:3b"), /*escalate*/ true, fakeClassifier{tier: T2}, fakeEscalator{})
    require.Equal(t, Escalate, r.Route(context.Background(), "stand up two agents and a review pipeline").Mode)
}
func TestRouter_BelowTierDegradesWhenEscalateOff(t *testing.T) {
    r := NewRouter(modelTier("qwen2.5-coder:3b"), false, fakeClassifier{tier: T2}, nil)
    d := r.Route(context.Background(), "compose a fleet")
    require.Equal(t, Degrade, d.Mode)
    require.NotEmpty(t, d.OperatorMessage)
}
```

- [ ] **Step 2: Implement**

```go
type Tier int
const ( T0 Tier = iota; T1; T2 )

func modelTier(model string) Tier { /* the spec's model→tier table; conservative default T0 */ }

// Classifier buckets a request's needed tier with one cheap T0 call (the
// llm.Completer / Classify seam — any model can do it).
type Classifier interface{ NeededTier(ctx context.Context, line string) Tier }

// Escalator drafts a plan with headless Claude (`claude -p`), returning the SAME
// confirm-gate tool calls a local plan would. Only the rare hard plan spends
// tokens; execution is still local warden calls.
type Escalator interface{ Plan(ctx context.Context, line string, tools []llm.ToolSchema) ([]ToolCall, error) }

type RouteMode int
const ( PlanLocal RouteMode = iota; Escalate; Degrade )

type Router struct { modelTier Tier; escalate bool; cls Classifier; esc Escalator }
func (r *Router) Route(ctx context.Context, line string) Route { /* compare, return mode + message */ }
```

The headless-Claude escalator shells `claude -p` the same bounded way `lifecycle.runClaudeP` does (lifecycle.go:31) — reuse that pattern; the orchestrator runs client-side so it spawns its own bounded `claude -p`. Wire `Router` into `Session.Handle` *before* the first `Chat` turn. Keep the classifier cheap and best-effort: if it errors, default to attempting locally (the gate is still the backstop) rather than blocking the operator.

- [ ] **Step 3: Run → pass → commit**

Commit: `feat(orchestrator): capability-tier routing (pre-classify → escalate-to-Claude or degrade)`.

---

## Task 5: `wd orch` command + config keys

**Files:** New `internal/cli/orch.go`, `internal/cli/orch_test.go`; modified `internal/cli/root.go`, `internal/config/config.go`

- [ ] **Step 1: Config keys (TDD against `config_test.go` patterns)**

Add three keys with defaults + descriptions + accessors, mirroring the existing `local_llm*` block (config.go:61-64, 121-124, 175-178):

  - `orchestrator` (bool, default `false`) — whether the cockpit master pane *starts* in orchestrator mode (consumed in Phase C; defined here so the surface is complete).
  - `local_llm_escalate` (bool, default `true`) — escalate an over-tier planning step to headless Claude; off ⇒ degrade.
  - `local_llm_tier` (string, default `"auto"`) — explicit model tier override (`auto`|`t0`|`t1`|`t2`).

Tests: defaults present after `config.Load` of an empty file; round-trips through `config get/set`.

- [ ] **Step 2: The command (`orch.go`)**

```go
func newOrchCmd() *cobra.Command {
    return &cobra.Command{
        Use:     "orch",
        Aliases: []string{"orchestrator"},
        Short:   "Natural-language conductor for agents, pipelines, and the git/check lifecycle (local LLM).",
        RunE: func(cmd *cobra.Command, _ []string) error {
            cfg := config.Load(configPathFor(cmd))
            if !cfg.GetLocalLLM() {
                return errors.New("orchestrator requires local_llm: true (it has no deterministic fallback — it's an interactive surface)")
            }
            cl := clientFor(cmd)                                  // same daemon client the CLI uses
            chat := llm.NewOllama(cfg.LocalLLMURL, cfg.LocalLLMModel, cfg.LocalLLMTimeoutDuration())
            sess := orchestrator.NewSession(chat, cl, /*registry*/ orchestrator.NewRegistry(),
                orchestrator.NewGate(cmd.InOrStdin(), cmd.OutOrStdout()),
                orchestrator.NewRouterFromConfig(cfg))
            return orchestrator.RunREPL(cmd.Context(), sess, cmd.InOrStdin(), cmd.OutOrStdout())
        },
    }
}
```

`RunREPL` is a thin read-line → `Session.Handle` → print loop in `session.go` (no `!` handling yet — Phase C adds that). Register in `root.go:45` next to `newTUICmd()`.

- [ ] **Step 3: Test the guard + wiring**

  - `wd orch` with `local_llm: false` returns the requires-local_llm error (no client call attempted).
  - command is registered on the root and resolves via its `orch`/`orchestrator` names.
  - a scripted-stdin REPL run against a `fakeChatter`+`fakeDaemon` (inject via a test constructor) produces expected output and exits cleanly on EOF.

- [ ] **Step 4: Full suite + commit**

```bash
cd /home/srjn45/dev/warden && go test ./... && go build ./...
```

Commit: `feat(cli): wd orch REPL — the orchestrator's standalone entrypoint + config keys`.

---

## Summary

Five TDD tasks, all additive; the only edits to existing files are one `AddCommand` line and three config keys.

1. ✅ `Daemon` seam + tool registry — warden's verbs as model-callable functions, read/mutate split, **no code-editing tool** (asserted).
2. ✅ Confirm gate — render mutations, `[a]pprove [e]dit [r]eject`, reject-by-default, batch-as-one.
3. ✅ The loop — auto-run reads, gate mutations, feed results back, bounded recovery + turn budget.
4. ✅ Tier routing — pre-classify → plan-local | escalate-to-Claude | degrade; model→tier table.
5. ✅ `wd orch` command + `orchestrator`/`local_llm_escalate`/`local_llm_tier` config.

**Milestone:** `wd orch` is usable standalone in any terminal — compose multi-agent arrangements from natural language, confirm before execute, with honest degradation when the model is under-tier.

**Deferred to C:** the embedded PTY shell, `!` passthrough, capture buffer, and the cockpit pane-command change. **Deferred to D:** the fleet-summarization NL verbs (the read tools exist here; the supervision UX is D). **Standalone-auth open question** (does `wd orch` read `~/.warden/daemon.env` like the rest of the CLI?) is settled in Task 5 by reusing `clientFor` — verify the token path there.
