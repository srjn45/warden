# Warden Orchestrator — Phase A: Tool-Calling Seam Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend `internal/llm` from a single-shot `Completer` to a multi-turn, **tool-calling** seam (`Chatter`), backed by Ollama's `/api/chat` endpoint — the net-new infrastructure the orchestrator's NL→tool-call loop (Phase B) rides on. This phase ships *only* the client seam: no daemon wiring, no orchestrator loop, no user surface.

**Design spec:** `docs/superpowers/specs/2026-06-25-warden-orchestrator-design.md` (Phase A)

**Architecture:** Three additive pieces inside the existing `internal/llm` package — nothing existing changes behavior:

1. **Chat types + `Chatter` interface** (`internal/llm/chat.go`): `Message` / `ToolSchema` / `ToolCall` / `Reply` plus the one-method `Chatter` seam, sitting *alongside* the existing `Completer` (`llm.go:16`). A robust `arguments` decoder (object **or** stringified-JSON) lives here because small models stringify tool args inconsistently.
2. **`Chat` on `*Ollama`** (`internal/llm/ollama_chat.go`): the same tiny-client discipline as `Complete` (`ollama.go:64`) — non-streaming, hard timeout, 1 MiB read cap, errors so the caller falls back — but posting to `/api/chat` with a `tools` array and decoding `message.tool_calls`. `*Ollama` ends up satisfying **both** `Completer` and `Chatter`, so no new constructor and no daemon change is needed in this phase.
3. **Faithful surfacing, not loop-recovery** (same file): `Chat` returns exactly what the model produced — prose, tool calls, or both — and never crashes on a degenerate/empty/malformed reply. The *recovery* policy (unknown-tool, retry budget, narrate-instead-of-call) is **Phase B's loop**, not here; Phase A's contract is "decode robustly and report truthfully."

**Tech Stack:** Go 1.26+, stdlib `net/http` + `encoding/json` only (matches the existing Ollama client). No new dependencies. Reuses the brain spec's `local_llm*` config — **no new config keys in this phase.**

**Scope guard:** This phase does not touch the daemon, CLI, MCP, or TUI, and adds no orchestrator package. If a task here needs any of those, it belongs in Phase B — stop and re-scope.

---

## File Structure

### New Files
- `internal/llm/chat.go` — `Message`, `Role`, `ToolSchema`, `ToolCall`, `Reply`, the `Chatter` interface, and `decodeArgs`
- `internal/llm/chat_test.go` — `decodeArgs` table tests + a compile-time `Chatter` contract assertion
- `internal/llm/ollama_chat.go` — `(*Ollama).Chat`, the Ollama `/api/chat` request/response structs, and the request builders
- `internal/llm/ollama_chat_test.go` — httptest-driven `Chat` tests (tool call, prose, both, error paths, defensive decode)

### Modified Files
- None. `*Ollama` gains a method in a new file; `llm.go` and `ollama.go` are untouched. (If `go doc` package overview needs a line about the new seam, that one-line doc edit to `llm.go` is allowed.)

---

## Task 1: Define the `Chatter` Seam + Chat Types

**Files:**
- New: `internal/llm/chat.go`
- Test: `internal/llm/chat_test.go`

The seam must (a) express a multi-turn conversation with tool definitions and tool results, and (b) decode model-emitted tool `arguments` whether they arrive as a JSON object (Ollama's normal form) or as a stringified JSON blob (what some small models emit). The second is the reliability footgun, so it gets its own tested helper.

- [ ] **Step 1: Write tests for `decodeArgs` and the interface contract**

Create `internal/llm/chat_test.go`:

```go
package llm

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeArgs_Object(t *testing.T) {
	got, err := decodeArgs(json.RawMessage(`{"type":"development","prompt":"x"}`))
	require.NoError(t, err)
	require.Equal(t, "development", got["type"])
	require.Equal(t, "x", got["prompt"])
}

func TestDecodeArgs_StringifiedObject(t *testing.T) {
	// Some small models emit arguments as a JSON *string*, not an object.
	got, err := decodeArgs(json.RawMessage(`"{\"type\":\"docs\"}"`))
	require.NoError(t, err)
	require.Equal(t, "docs", got["type"])
}

func TestDecodeArgs_EmptyAndNull(t *testing.T) {
	for _, raw := range []string{``, `null`, `{}`, `""`} {
		got, err := decodeArgs(json.RawMessage(raw))
		require.NoError(t, err, "empty/null args are a valid no-arg call, not an error: %q", raw)
		require.NotNil(t, got, "always return a usable (possibly empty) map: %q", raw)
	}
}

func TestDecodeArgs_GarbageErrors(t *testing.T) {
	_, err := decodeArgs(json.RawMessage(`{not json`))
	require.Error(t, err, "un-parseable args must error so the loop can recover")
}

// Compile-time proof the concrete client will satisfy the seam (filled in Task 2).
var _ Chatter = (*Ollama)(nil)

// A trivial fake keeps Phase B unblocked before the real client lands.
type fakeChatter struct{ reply Reply }

func (f fakeChatter) Chat(context.Context, []Message, []ToolSchema) (Reply, error) {
	return f.reply, nil
}

func TestFakeChatterSatisfiesSeam(t *testing.T) {
	var c Chatter = fakeChatter{reply: Reply{Text: "ok"}}
	r, err := c.Chat(context.Background(), nil, nil)
	require.NoError(t, err)
	require.Equal(t, "ok", r.Text)
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd internal/llm && go test -run 'DecodeArgs|Chatter|FakeChatter' -v
```

Expected: FAIL — `Chatter`, `decodeArgs`, the chat types, and the `var _ Chatter = (*Ollama)(nil)` assertion don't exist yet (the last won't compile until Task 2, so this task's package won't build until both land — that's fine; Task 2 closes it).

- [ ] **Step 3: Write `chat.go`**

Create `internal/llm/chat.go`:

```go
package llm

import (
	"context"
	"encoding/json"
	"fmt"
)

// Role identifies who produced a chat Message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool" // a tool result fed back to the model
)

// Message is one turn in a tool-calling conversation. ToolCalls is set on an
// assistant turn that invoked tools; ToolName is set on a RoleTool turn to say
// which tool the Content is a result for.
type Message struct {
	Role      Role
	Content   string
	ToolCalls []ToolCall
	ToolName  string
}

// ToolSchema describes a callable tool to the model: a name, a human description,
// and a JSON-Schema object for its parameters. Implementations pass these to the
// model so it can choose and shape a call.
type ToolSchema struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON Schema (an "object" schema)
}

// ToolCall is the model's request to invoke a tool with decoded arguments. The
// caller (Phase B's loop) is responsible for validating Name against its registry
// and Args against the schema — Chatter only reports what the model asked for.
type ToolCall struct {
	Name string
	Args map[string]any
}

// Reply is one assistant turn. It may carry prose (Text), tool calls, or both;
// an empty Reply (no text, no calls) is a degenerate-but-valid model response the
// caller decides how to handle — Chat does not treat it as an error.
type Reply struct {
	Text      string
	ToolCalls []ToolCall
}

// Chatter drives a multi-turn, tool-calling conversation. Given the running
// message history and the available tools, it returns the model's next turn.
// Implementations must honour ctx (warden bounds every call with a hard timeout)
// and return an error on any transport/HTTP/decode failure so the caller falls
// back — never a silently-empty success masking a failure.
type Chatter interface {
	Chat(ctx context.Context, msgs []Message, tools []ToolSchema) (Reply, error)
}

// decodeArgs turns a model-emitted tool-call arguments blob into a map. Ollama
// normally sends a JSON object, but some small models stringify it; we accept
// both, and treat empty/null/"" as a valid no-argument call (an empty map). Only
// genuinely un-parseable input errors, so the loop can recover that turn.
func decodeArgs(raw json.RawMessage) (map[string]any, error) {
	trimmed := trimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" || string(trimmed) == `""` {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(trimmed, &m); err == nil {
		return m, nil
	}
	// Maybe it's a JSON string *containing* JSON. Unwrap one level and retry.
	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		if err := json.Unmarshal([]byte(s), &m); err == nil {
			return m, nil
		}
	}
	return nil, fmt.Errorf("decode tool arguments: not a JSON object or stringified object: %s", trimmed)
}

// trimSpace trims leading/trailing ASCII whitespace from a RawMessage without a
// string copy (json.RawMessage is a []byte).
func trimSpace(b json.RawMessage) json.RawMessage {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\t' || b[j-1] == '\n' || b[j-1] == '\r') {
		j--
	}
	return b[i:j]
}
```

(The `var _ Chatter = (*Ollama)(nil)` line in the test stays red until Task 2 — leave it; do not stub a `Chat` method here.)

- [ ] **Step 4: Confirm the types compile in isolation**

```bash
cd internal/llm && go build ./... 2>&1 | grep -v 'Chat' || true
go vet ./... 2>&1 | grep -v 'missing method Chat' || true
```

Expected: the only outstanding error is the missing `(*Ollama).Chat` from the contract assertion — closed in Task 2.

- [ ] **Step 5: Commit**

```bash
git add internal/llm/chat.go internal/llm/chat_test.go
git commit -m "feat(llm): add Chatter tool-calling seam alongside Completer

Define Message/Role/ToolSchema/ToolCall/Reply and the one-method Chatter
interface for multi-turn, tool-calling conversations — the seam the warden
orchestrator's NL-to-tool-call loop will ride on. decodeArgs accepts tool
arguments as a JSON object or a stringified object (small models emit both)
and treats empty/null as a valid no-arg call. No behavior change to the
existing single-shot Completer path.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: Implement `Chat` on `*Ollama` via `/api/chat`

**Files:**
- New: `internal/llm/ollama_chat.go`
- Test: `internal/llm/ollama_chat_test.go`

Mirror the `Complete` discipline (`ollama.go:64-96`): one non-streaming request, JSON in/out, hard timeout via the shared `o.http` client, response read under `maxResponseBytes` (`ollama.go:24`), every failure returned as an error. `*Ollama` already holds `url`, `model`, and `http` (`ollama.go:29-33`); reuse them.

- [ ] **Step 1: Write httptest tests for the chat path**

Create `internal/llm/ollama_chat_test.go`:

```go
package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOllamaChat_ToolCall(t *testing.T) {
	var gotReq ollamaChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/chat", r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &gotReq))
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{Message: ollamaChatMessage{
			Role: "assistant",
			ToolCalls: []ollamaToolCall{{Function: ollamaToolCallFunction{
				Name: "spawn_agent", Arguments: json.RawMessage(`{"type":"development","prompt":"refactor"}`),
			}}},
		}})
	}))
	defer srv.Close()

	msgs := []Message{{Role: RoleUser, Content: "spawn a dev agent to refactor"}}
	tools := []ToolSchema{{
		Name: "spawn_agent", Description: "start an agent",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"type": map[string]any{"type": "string"}, "prompt": map[string]any{"type": "string"},
		}},
	}}
	reply, err := NewOllama(srv.URL, "qwen2.5-coder:3b", time.Second).Chat(context.Background(), msgs, tools)
	require.NoError(t, err)
	require.Len(t, reply.ToolCalls, 1)
	require.Equal(t, "spawn_agent", reply.ToolCalls[0].Name)
	require.Equal(t, "development", reply.ToolCalls[0].Args["type"])
	// Request carried the tools and the non-streaming flag.
	require.False(t, gotReq.Stream)
	require.Len(t, gotReq.Tools, 1)
	require.Equal(t, "function", gotReq.Tools[0].Type)
	require.Equal(t, "spawn_agent", gotReq.Tools[0].Function.Name)
	require.Equal(t, "qwen2.5-coder:3b", gotReq.Model)
}

func TestOllamaChat_Prose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{Message: ollamaChatMessage{
			Role: "assistant", Content: "Two agents are running; none are blocked.",
		}})
	}))
	defer srv.Close()
	reply, err := NewOllama(srv.URL, "m", time.Second).Chat(context.Background(), nil, nil)
	require.NoError(t, err)
	require.Empty(t, reply.ToolCalls)
	require.Contains(t, reply.Text, "Two agents")
}

func TestOllamaChat_TextAndToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{Message: ollamaChatMessage{
			Role: "assistant", Content: "I'll list them.",
			ToolCalls: []ollamaToolCall{{Function: ollamaToolCallFunction{
				Name: "list_agents", Arguments: json.RawMessage(`{}`),
			}}},
		}})
	}))
	defer srv.Close()
	reply, err := NewOllama(srv.URL, "m", time.Second).Chat(context.Background(), nil, nil)
	require.NoError(t, err)
	require.Equal(t, "I'll list them.", reply.Text)
	require.Len(t, reply.ToolCalls, 1)
	require.Equal(t, "list_agents", reply.ToolCalls[0].Name)
	require.Empty(t, reply.ToolCalls[0].Args)
}

func TestOllamaChat_StringifiedArgsAreDecoded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{Message: ollamaChatMessage{
			Role: "assistant",
			ToolCalls: []ollamaToolCall{{Function: ollamaToolCallFunction{
				Name: "commit", Arguments: json.RawMessage(`"{\"message\":\"wip\"}"`),
			}}},
		}})
	}))
	defer srv.Close()
	reply, err := NewOllama(srv.URL, "m", time.Second).Chat(context.Background(), nil, nil)
	require.NoError(t, err)
	require.Equal(t, "wip", reply.ToolCalls[0].Args["message"])
}

func TestOllamaChat_BadArgsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{Message: ollamaChatMessage{
			ToolCalls: []ollamaToolCall{{Function: ollamaToolCallFunction{
				Name: "x", Arguments: json.RawMessage(`{nope`),
			}}},
		}})
	}))
	defer srv.Close()
	_, err := NewOllama(srv.URL, "m", time.Second).Chat(context.Background(), nil, nil)
	require.Error(t, err, "un-parseable tool args must surface as an error, not a silent empty call")
}

func TestOllamaChat_Non200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := NewOllama(srv.URL, "missing", time.Second).Chat(context.Background(), nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "404")
}

func TestOllamaChat_APIErrorField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{Error: "model 'foo' not found"})
	}))
	defer srv.Close()
	_, err := NewOllama(srv.URL, "foo", time.Second).Chat(context.Background(), nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestOllamaChat_TimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	start := time.Now()
	_, err := NewOllama(srv.URL, "slow", 50*time.Millisecond).Chat(context.Background(), nil, nil)
	require.Error(t, err)
	require.Less(t, time.Since(start), 2*time.Second, "the hard timeout must bound the chat call")
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd internal/llm && go test -run TestOllamaChat -v
```

Expected: FAIL/compile-error — `Chat` and the `ollamaChat*` types don't exist.

- [ ] **Step 3: Write `ollama_chat.go`**

Create `internal/llm/ollama_chat.go`:

```go
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// --- Ollama /api/chat wire types ---

type ollamaChatRequest struct {
	Model    string              `json:"model"`
	Messages []ollamaChatMessage `json:"messages"`
	Tools    []ollamaTool        `json:"tools,omitempty"`
	Stream   bool                `json:"stream"`
}

type ollamaChatMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolName  string           `json:"tool_name,omitempty"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

type ollamaTool struct {
	Type     string         `json:"type"` // always "function"
	Function ollamaFunction `json:"function"`
}

type ollamaFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type ollamaToolCall struct {
	Function ollamaToolCallFunction `json:"function"`
}

type ollamaToolCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ollamaChatResponse struct {
	Message ollamaChatMessage `json:"message"`
	Error   string            `json:"error"`
}

// Chat sends the conversation + tool schemas to Ollama's /api/chat and returns
// the model's next turn. Non-streaming so the whole reply is one JSON object;
// every transport/HTTP/API/decode failure (including un-parseable tool
// arguments) is returned as an error so the caller falls back, read under the
// shared maxResponseBytes cap and the client's hard timeout.
func (o *Ollama) Chat(ctx context.Context, msgs []Message, tools []ToolSchema) (Reply, error) {
	reqBody := ollamaChatRequest{
		Model:    o.model,
		Messages: toOllamaMessages(msgs),
		Tools:    toOllamaTools(tools),
		Stream:   false,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return Reply{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return Reply{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.http.Do(req)
	if err != nil {
		return Reply{}, fmt.Errorf("ollama chat: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return Reply{}, fmt.Errorf("ollama read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Reply{}, fmt.Errorf("ollama chat: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out ollamaChatResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return Reply{}, fmt.Errorf("ollama decode: %w", err)
	}
	if out.Error != "" {
		return Reply{}, fmt.Errorf("ollama: %s", out.Error)
	}

	reply := Reply{Text: out.Message.Content}
	for _, tc := range out.Message.ToolCalls {
		args, err := decodeArgs(tc.Function.Arguments)
		if err != nil {
			return Reply{}, fmt.Errorf("tool call %q: %w", tc.Function.Name, err)
		}
		reply.ToolCalls = append(reply.ToolCalls, ToolCall{Name: tc.Function.Name, Args: args})
	}
	return reply, nil
}

// toOllamaMessages converts warden's Message slice to the wire form. An
// assistant turn's prior tool calls are re-serialised so a multi-turn loop can
// replay them; a RoleTool turn carries its tool_name.
func toOllamaMessages(msgs []Message) []ollamaChatMessage {
	out := make([]ollamaChatMessage, 0, len(msgs))
	for _, m := range msgs {
		om := ollamaChatMessage{Role: string(m.Role), Content: m.Content, ToolName: m.ToolName}
		for _, tc := range m.ToolCalls {
			args, _ := json.Marshal(tc.Args) // a map always marshals
			om.ToolCalls = append(om.ToolCalls, ollamaToolCall{
				Function: ollamaToolCallFunction{Name: tc.Name, Arguments: args},
			})
		}
		out = append(out, om)
	}
	return out
}

// toOllamaTools converts tool schemas to the wire form (type:"function").
func toOllamaTools(tools []ToolSchema) []ollamaTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]ollamaTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, ollamaTool{Type: "function", Function: ollamaFunction{
			Name: t.Name, Description: t.Description, Parameters: t.Parameters,
		}})
	}
	return out
}
```

- [ ] **Step 4: Run to verify it passes (incl. Task 1's contract assertion)**

```bash
cd internal/llm && go test ./... -v
```

Expected: PASS — all `TestOllamaChat_*`, the Task-1 `decodeArgs`/`fakeChatter` tests, `var _ Chatter = (*Ollama)(nil)` now compiles, and the existing `Complete` tests are untouched.

- [ ] **Step 5: Commit**

```bash
git add internal/llm/ollama_chat.go internal/llm/ollama_chat_test.go
git commit -m "feat(llm): implement Chat on Ollama via /api/chat

*Ollama now satisfies Chatter as well as Completer: one non-streaming
/api/chat request carrying the message history and tool schemas, decoding
message.tool_calls into ToolCall values (arguments via decodeArgs, so a
stringified-JSON blob from a small model is handled). Same discipline as
Complete — hard timeout, 1 MiB read cap, every transport/HTTP/API/decode
failure (including un-parseable tool args) returned as an error so the
caller falls back. No new constructor or config; reuses the local_llm* seam.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: Package Verification & Doc Pass

**Files:** all of `internal/llm`.

- [ ] **Step 1: Full package suite, vet, build**

```bash
cd /home/srjn45/dev/warden
go vet ./internal/llm/...
go test ./internal/llm/...
go build ./...
```

Expected: exit 0; the new seam links cleanly and nothing outside `internal/llm` changed.

- [ ] **Step 2: Confirm both interfaces are satisfied by one type**

```bash
cd internal/llm && cat > /tmp/iface_check.go <<'EOF'
//go:build ignore
package llm
var _ Completer = (*Ollama)(nil)
var _ Chatter   = (*Ollama)(nil)
EOF
go vet ./... && rm -f /tmp/iface_check.go
```

Expected: clean. (The assertions also live permanently in the test files; this is a quick belt-and-suspenders.)

- [ ] **Step 3: One-line package doc note (optional)**

If `llm.go`'s package comment (`llm.go:1-8`) reads as Completer-only, add a single sentence noting the package now also exposes a tool-calling `Chatter` seam for the orchestrator. Keep it to one line; do not restructure the comment.

- [ ] **Step 4: Run the whole repo suite once**

```bash
cd /home/srjn45/dev/warden && go test ./...
```

Expected: exit 0 — Phase A is additive, so no other package should move.

- [ ] **Step 5: Commit (only if the doc line was touched)**

```bash
git add internal/llm/llm.go
git commit -m "docs(llm): note the Chatter tool-calling seam in the package doc

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Summary

Three tasks, each TDD (write test → fail → implement → pass → commit), all inside `internal/llm` and fully additive:

1. ✅ `chat.go` — `Message`/`Role`/`ToolSchema`/`ToolCall`/`Reply` + the `Chatter` seam + `decodeArgs` (object-or-stringified, empty=no-arg, garbage=error).
2. ✅ `ollama_chat.go` — `(*Ollama).Chat` over `/api/chat`, same tiny-client discipline as `Complete`; `*Ollama` now satisfies both `Completer` and `Chatter`. No new constructor, no daemon wiring, no new config.
3. ✅ Package verification, dual-interface assertion, optional one-line doc.

**Deliberately deferred to Phase B** (do **not** build here): the orchestrator loop, the tool registry mapping warden's MCP surface to `[]ToolSchema`, the confirm gate, unknown-tool / retry-budget / narrate-instead-of-call *recovery* policy, capability-tier routing, and any daemon/CLI/MCP/TUI wiring. Phase A's contract stops at "decode robustly and report the model's turn truthfully"; everything that *acts* on that turn is Phase B.

**What Phase B inherits:** a `Chatter` it gets by type-asserting the already-constructed `local_llm` provider (`*Ollama` satisfies it), plus the `fakeChatter` test double so the loop can be built and tested without a live model.
