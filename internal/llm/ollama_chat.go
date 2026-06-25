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
