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
