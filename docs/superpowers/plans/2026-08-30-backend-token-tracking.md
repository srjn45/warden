# Backend-Specific Token Tracking Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Refactor Warden's context token and spend usage tracking — currently hardcoded to parse Claude Code's JSONL format — to support multiple backends. This will allow Warden to track token context sizes and expenditures for the Antigravity backend and pave the way for other backends (Aider, OpenCode, Codex).

**Architecture:** 
1. Define a backend-agnostic `TokenParser` interface in `internal/ctxtokens` and `internal/spend`.
2. Extract the existing Claude Code logic into a `ClaudeParser`.
3. Implement an `AntigravityParser`. Since Antigravity logs (`transcript.jsonl`) are fundamentally different and currently do not emit explicit token usage, this parser will calculate an approximated token count based on byte/word heuristics of the `content` and `thinking` fields, or parse explicit usage if Antigravity introduces it.
4. Modify `pollerDeps.ContextTokens` and `pollerDeps.TranscriptUsage` to dispatch reading to the correct parser based on the agent's `Backend` property.

**Tech Stack:** Go (stdlib `bufio`/`encoding/json`).

---

## File Structure

**Modify:**
- `internal/ctxtokens/ctxtokens.go` — Export a parser interface and registry.
- `internal/daemon/poller_deps.go` — Update `ContextTokens` and `TranscriptUsage` to dispatch based on `s.Backend`.
- `internal/spend/parse.go` — Export a spend parser interface and registry.

**Create:**
- `internal/ctxtokens/claude.go` — Move existing `LatestContextTokens` Claude logic here.
- `internal/ctxtokens/antigravity.go` — Implement Antigravity token heuristic parser.
- `internal/spend/claude.go` — Move existing `ParseUsage` Claude logic here.
- `internal/spend/antigravity.go` — Implement Antigravity spend parser (returns 0 cost for free local models or estimates).

---

## Tasks

### Task 1: Refactor `ctxtokens` for Backend Parsers
- [ ] In `internal/ctxtokens`, define a `TokenParser` interface with a method `LatestContextTokens(r io.Reader) (tokens int, ok bool)`.
- [ ] Move the existing Claude Code implementation into `internal/ctxtokens/claude.go` (e.g., as `ClaudeParser`).
- [ ] Create a `GetParser(backend string) TokenParser` factory function that returns `ClaudeParser` by default.
- [ ] Update `internal/ctxtokens/ctxtokens_test.go` to test the new parser structure.

### Task 2: Implement Antigravity Context Parser
- [ ] Create `internal/ctxtokens/antigravity.go` with an `AntigravityParser`.
- [ ] Implement `LatestContextTokens` to scan Antigravity's `transcript.jsonl` format.
  - *Detail:* Look for `"type":"PLANNER_RESPONSE"` and `"type":"USER_INPUT"`. 
  - *Detail:* Since exact token usage is missing, use a safe heuristic (e.g., `(len(content) + len(thinking)) / 4` to estimate tokens) as a placeholder, accumulating this over the conversation.
- [ ] Wire it into the `GetParser` factory for backend `"antigravity"`.
- [ ] Write unit tests for `AntigravityParser` using sample Antigravity JSONL payloads.

### Task 3: Refactor Spend Tracking
- [ ] Similarly, in `internal/spend`, define a `SpendParser` interface for `ParseUsage(r io.Reader) (Usage, bool)`.
- [ ] Move the existing Claude-specific spend parser into `internal/spend/claude.go`.
- [ ] Create `internal/spend/antigravity.go`. Since Antigravity might run on a flat-rate or local model, this can initially return `0` cost, or use a heuristic cost algorithm based on the estimated tokens.
- [ ] Add a `GetParser(backend string)` factory in the `spend` package.

### Task 4: Wire the Daemon Poller
- [ ] In `internal/daemon/poller_deps.go`, update `ContextTokens`:
  - Fetch the appropriate parser using `ctxtokens.GetParser(s.Backend)`.
  - Call `parser.LatestContextTokens(f)`.
- [ ] In `internal/daemon/poller_deps.go`, update `TranscriptUsage`:
  - Fetch the appropriate parser using `spend.GetParser(s.Backend)`.
  - Call `parser.ParseUsage(f)`.
- [ ] Run the test suite (`make test` or `go test ./...`) to ensure regressions are not introduced for existing Claude agents.
