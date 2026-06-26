package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

// ToolArgError reports that the model emitted a tool call whose arguments could
// not be decoded into an object. Unlike a transport or HTTP failure it is
// *recoverable*: the caller should feed the problem back to the model and let it
// retry the turn, rather than treating the model as unavailable. It carries the
// offending tool's name so the nudge can be specific.
type ToolArgError struct {
	Tool string
	Err  error
}

func (e *ToolArgError) Error() string { return fmt.Sprintf("tool call %q: %v", e.Tool, e.Err) }
func (e *ToolArgError) Unwrap() error { return e.Err }

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

// inlineCall is the JSON shape small models emit when they put a tool call in
// the assistant *content* instead of the structured tool_calls field. Different
// models name the arguments key differently, so we accept both spellings.
type inlineCall struct {
	Name       string          `json:"name"`
	Arguments  json.RawMessage `json:"arguments"`
	Parameters json.RawMessage `json:"parameters"`
}

// SalvageToolCalls recovers tool calls a small model emitted as plain content —
// a JSON object (or array of them) shaped like {"name":...,"arguments":{...}} —
// instead of in the structured tool_calls field. This is a common qwen/Ollama
// failure mode that otherwise surfaces to the operator as raw JSON and runs
// nothing. It returns the recovered calls and the residual prose (the content
// with the salvaged JSON removed).
//
// It is deliberately conservative: it fires only when the *entire* trimmed
// content — after stripping ```json fences and <tool_call> tags — parses as a
// tool-call-shaped object/array with a non-empty name, so ordinary prose (or
// prose that merely mentions JSON) is never misread as a call.
func SalvageToolCalls(content string) (calls []ToolCall, residual string) {
	t := stripToolCallWrappers(content)
	if t == "" {
		return nil, content
	}
	var list []inlineCall
	if err := json.Unmarshal([]byte(t), &list); err != nil {
		var one inlineCall
		if err := json.Unmarshal([]byte(t), &one); err != nil {
			return nil, content
		}
		list = []inlineCall{one}
	}
	for _, ic := range list {
		if len(trimSpaceStr(ic.Name)) == 0 {
			return nil, content // not actually a tool call — leave content untouched
		}
		raw := ic.Arguments
		if len(raw) == 0 {
			raw = ic.Parameters
		}
		args, err := decodeArgs(raw)
		if err != nil {
			return nil, content
		}
		calls = append(calls, ToolCall{Name: ic.Name, Args: args})
	}
	if len(calls) == 0 {
		return nil, content
	}
	return calls, "" // the whole content WAS the call(s)
}

// stripToolCallWrappers removes the ```json … ``` fences and <tool_call> … tags
// small models wrap a content-embedded tool call in, returning the trimmed inner
// text. Applied fence-then-tag so either nesting order is unwrapped.
func stripToolCallWrappers(s string) string {
	t := trimSpaceStr(s)
	if rest, ok := strings.CutPrefix(t, "```"); ok {
		rest = strings.TrimPrefix(rest, "json")
		rest = strings.TrimPrefix(rest, "JSON")
		if i := strings.LastIndex(rest, "```"); i >= 0 {
			rest = rest[:i]
		}
		t = trimSpaceStr(rest)
	}
	t = trimSpaceStr(strings.TrimSuffix(strings.TrimPrefix(t, "<tool_call>"), "</tool_call>"))
	return t
}

// trimSpaceStr trims leading/trailing whitespace from a string.
func trimSpaceStr(s string) string { return strings.TrimSpace(s) }

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
