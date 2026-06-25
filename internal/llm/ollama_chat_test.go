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
