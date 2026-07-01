package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultOllamaURL is the local Ollama endpoint used when none is configured.
const DefaultOllamaURL = "http://localhost:11434"

// defaultTimeout caps a generate request when the caller passes none. Local
// inference must never outlast its caller's patience — past this the call errors
// and the caller falls back.
const defaultTimeout = 20 * time.Second

// maxResponseBytes bounds how much of a response we read, so a misbehaving or
// streaming endpoint can't make warden buffer unbounded memory.
const maxResponseBytes = 1 << 20 // 1 MiB

// Ollama is a Completer backed by an Ollama server's /api/generate endpoint. It
// is deliberately tiny — one non-streaming request, JSON in and out. Construct it
// with NewOllama; the zero value is not usable.
type Ollama struct {
	url   string
	model string
	http  *http.Client
}

// NewOllama builds an Ollama completer. A blank url falls back to the local
// default; a non-positive timeout falls back to defaultTimeout and caps the whole
// request (connect + generate) so a stuck model degrades to the caller's fallback.
func NewOllama(url, model string, timeout time.Duration) *Ollama {
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	if url == "" {
		url = DefaultOllamaURL
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Ollama{url: url, model: model, http: &http.Client{Timeout: timeout}}
}

// ollamaTagsResponse is the shape of GET /api/tags: the locally-pulled models.
type ollamaTagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// InstalledModels returns the names of the models pulled into the local Ollama
// server (GET /api/tags), e.g. ["qwen3.5:2b", "qwen2.5-coder:7b"]. It exists so
// warden can verify that a configured local_llm.model is actually present before
// relying on it — otherwise every classify/summarize call 404s and silently
// escalates to a full Claude process. Transport/status/decode failures return an
// error so the caller degrades (treats the model set as unknown, not empty).
func (o *Ollama) InstalledModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.url+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama tags: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("ollama tags read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama tags: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out ollamaTagsResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("ollama tags decode: %w", err)
	}
	names := make([]string, 0, len(out.Models))
	for _, m := range out.Models {
		names = append(names, m.Name)
	}
	return names, nil
}

// ModelInstalled reports whether the configured model name is among installed.
// Ollama defaults an untagged reference to the ":latest" tag, so an untagged
// config (e.g. "llama3") matches an installed "llama3:latest" and vice versa. An
// empty configured name is never considered installed.
func ModelInstalled(configured string, installed []string) bool {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return false
	}
	norm := func(s string) string {
		if !strings.Contains(s, ":") {
			return s + ":latest"
		}
		return s
	}
	want := norm(configured)
	for _, m := range installed {
		if norm(strings.TrimSpace(m)) == want {
			return true
		}
	}
	return false
}

type ollamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

type ollamaResponse struct {
	Response string `json:"response"`
	Error    string `json:"error"`
}

// Complete sends prompt to Ollama and returns the model's text response. It uses
// the non-streaming form (stream:false) so the whole reply arrives as one JSON
// object. Any transport, HTTP-status, API, or decode failure is returned as an
// error so the caller falls back; the response is read under a hard byte cap.
func (o *Ollama) Complete(ctx context.Context, prompt string) (string, error) {
	body, err := json.Marshal(ollamaRequest{Model: o.model, Prompt: prompt, Stream: false})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama generate: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("ollama read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama generate: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out ollamaResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("ollama decode: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("ollama: %s", out.Error)
	}
	return out.Response, nil
}
