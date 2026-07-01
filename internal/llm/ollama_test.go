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

func TestOllamaCompleteHappyPath(t *testing.T) {
	var gotReq ollamaRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/generate", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &gotReq))
		_ = json.NewEncoder(w).Encode(ollamaResponse{Response: "development"})
	}))
	defer srv.Close()

	out, err := NewOllama(srv.URL, "qwen2.5-coder:7b", time.Second).Complete(context.Background(), "classify: build an API")
	require.NoError(t, err)
	require.Equal(t, "development", out)
	require.Equal(t, "qwen2.5-coder:7b", gotReq.Model)
	require.Equal(t, "classify: build an API", gotReq.Prompt)
	require.False(t, gotReq.Stream, "warden must request the non-streaming form")
}

func TestOllamaCompleteNon200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := NewOllama(srv.URL, "missing", time.Second).Complete(context.Background(), "x")
	require.Error(t, err)
	require.Contains(t, err.Error(), "404")
}

func TestOllamaCompleteAPIErrorField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ollamaResponse{Error: "model 'foo' not found, try pulling it first"})
	}))
	defer srv.Close()

	_, err := NewOllama(srv.URL, "foo", time.Second).Complete(context.Background(), "x")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestOllamaCompleteTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release // hold the request open past the client timeout
	}))
	defer srv.Close()
	defer close(release)

	start := time.Now()
	_, err := NewOllama(srv.URL, "slow", 50*time.Millisecond).Complete(context.Background(), "x")
	require.Error(t, err, "a model slower than the timeout must error so the caller falls back")
	require.Less(t, time.Since(start), 2*time.Second, "the hard timeout must bound the call")
}

func TestOllamaCompleteHonoursContextCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	_, err := NewOllama(srv.URL, "m", time.Minute).Complete(ctx, "x")
	require.Error(t, err)
}

func TestOllamaInstalledModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/tags", r.URL.Path)
		_, _ = io.WriteString(w, `{"models":[{"name":"qwen3.5:2b"},{"name":"qwen2.5-coder:7b"}]}`)
	}))
	defer srv.Close()

	got, err := NewOllama(srv.URL, "m", time.Second).InstalledModels(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"qwen3.5:2b", "qwen2.5-coder:7b"}, got)
}

func TestModelInstalled(t *testing.T) {
	installed := []string{"qwen3.5:2b", "qwen2.5-coder:7b", "llama3:latest"}
	require.True(t, ModelInstalled("qwen3.5:2b", installed), "exact match")
	require.True(t, ModelInstalled("llama3", installed), "untagged config matches :latest")
	require.True(t, ModelInstalled("llama3:latest", installed), "explicit :latest matches")
	require.False(t, ModelInstalled("qwen3:14b", installed), "a model not installed is reported missing")
	require.False(t, ModelInstalled("", installed), "empty config is never 'installed'")
}

func TestNewOllamaDefaultsAndNormalises(t *testing.T) {
	o := NewOllama("  http://example:1234/  ", "m", 0)
	require.Equal(t, "http://example:1234", o.url, "trailing slash and surrounding space are trimmed")

	d := NewOllama("", "m", -5*time.Second)
	require.Equal(t, DefaultOllamaURL, d.url, "a blank url falls back to the local default")
	require.Equal(t, defaultTimeout, d.http.Timeout, "a non-positive timeout falls back to the default")
}
