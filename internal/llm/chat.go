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
