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
				Name: "x", Arguments: json.RawMessage(`123`), // valid JSON, but not an args object
			}}},
		}})
	}))
	defer srv.Close()
	_, err := NewOllama(srv.URL, "m", time.Second).Chat(context.Background(), nil, nil)
	require.Error(t, err, "un-parseable tool args must surface as an error, not a silent empty call")
	var tae *ToolArgError
	require.ErrorAs(t, err, &tae, "a malformed-args error is typed so the caller can retry it")
	require.Equal(t, "x", tae.Tool)
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

// TestOllamaChat_SalvagesInlineToolCall covers the qwen failure mode in the
// screenshot: the model emits a tool call as plain content (no structured
// tool_calls). The salvage must promote it so the loop runs it instead of
// printing raw JSON and doing nothing.
func TestOllamaChat_SalvagesInlineToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{Message: ollamaChatMessage{
			Role:    "assistant",
			Content: `{"name":"spawn_agent","arguments":{"prompt":"review docs"}}`,
		}})
	}))
	defer srv.Close()
	reply, err := NewOllama(srv.URL, "m", time.Second).Chat(context.Background(), nil, nil)
	require.NoError(t, err)
	require.Len(t, reply.ToolCalls, 1)
	require.Equal(t, "spawn_agent", reply.ToolCalls[0].Name)
	require.Equal(t, "review docs", reply.ToolCalls[0].Args["prompt"])
	require.Empty(t, reply.Text, "the salvaged JSON must not also leak as prose")
}

func TestSalvageToolCalls(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string // expected tool name, "" ⇒ no salvage
	}{
		{"bare object", `{"name":"spawn_agent","arguments":{"prompt":"x"}}`, "spawn_agent"},
		{"parameters key", `{"name":"check","parameters":{"agent":"a1"}}`, "check"},
		{"json fenced", "```json\n{\"name\":\"push\",\"arguments\":{\"agent\":\"a1\"}}\n```", "push"},
		{"tool_call tagged", `<tool_call>{"name":"sync","arguments":{}}</tool_call>`, "sync"},
		{"array", `[{"name":"list_agents","arguments":{}}]`, "list_agents"},
		{"plain prose", "Two agents are running; none blocked.", ""},
		{"json mention in prose", `I would call {"name":"x"} but won't.`, ""},
		{"object without name", `{"arguments":{"prompt":"x"}}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls, residual := SalvageToolCalls(tc.content)
			if tc.want == "" {
				require.Empty(t, calls)
				require.Equal(t, tc.content, residual, "non-salvage must leave content untouched")
				return
			}
			require.Len(t, calls, 1)
			require.Equal(t, tc.want, calls[0].Name)
			require.Empty(t, residual)
		})
	}
}
