package savings

// Calibration replaces the generic 4-bytes/token guess with a factor measured
// against THIS workload, so the savings figures survive a skeptic's "4 is
// hand-wavy" objection. `wd savings --calibrate` counts the real bytes of a
// bounded sample of retained raw/kept output against Claude's count_tokens
// endpoint and derives an empirical bytes-per-token ratio, persisted next to the
// ledger. The daemon loads it (SetCalibration) so EstimateTokensLen prices new
// events by the measured ratio instead of the heuristic.
//
// The math (DeriveCalibration) is pure and takes an injected TokenCounter, so it
// is unit-testable with no network; only NewAnthropicCounter touches the API, and
// only --calibrate constructs one. Every other command works fully offline.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// CalibrationModel is the Claude model whose tokenizer the calibration measures
// against — the same model warden's saved tokens are priced at. Token counts are
// model-specific, so the report names it.
const CalibrationModel = "claude-opus-4-8"

// calibrationFile is the JSON sidecar (next to ledger.jsonl) holding the active
// empirical factor. A small single object, rewritten atomically.
const calibrationFile = "calibration.json"

// DefaultCalibrationCalls bounds the number of paid count_tokens calls a single
// --calibrate run makes by default. Calibration is a workload-shape measurement,
// not a census — a few dozen real samples pin the bytes-per-token ratio tightly,
// and the cap keeps the paid API spend predictable. Overridable via the flag.
const DefaultCalibrationCalls = 50

// Calibration is the persisted result of `wd savings --calibrate`: the empirical
// bytes-per-token factor plus the basis behind it (how many samples, the total
// bytes and tokens they summed to, the model, and when) so the report can state
// the figure is measured, not guessed.
type Calibration struct {
	BytesPerToken float64   `json:"bytes_per_token"`
	Samples       int       `json:"samples"`
	SampleBytes   int       `json:"sample_bytes"`
	SampleTokens  int       `json:"sample_tokens"`
	Model         string    `json:"model"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// TokenCounter counts the tokens in a string against a real tokenizer. The
// production implementation calls Claude's count_tokens endpoint; tests inject a
// deterministic fake so the derivation math is verified with no network.
type TokenCounter interface {
	CountTokens(ctx context.Context, text string) (int, error)
}

// DeriveCalibration counts the tokens in up to maxCalls retained samples and
// derives the empirical factor = sum(sampleBytes) / sum(actualTokens). It bounds
// the number of (paid) counter calls to maxCalls, skips empty samples, and treats
// any per-sample counter error as fatal — a partial sum would silently bias the
// factor, so it is better to fail loudly and leave the persisted factor untouched.
// Pure except for the injected counter: no network, no disk.
func DeriveCalibration(ctx context.Context, counter TokenCounter, samples []string, maxCalls int) (Calibration, error) {
	if counter == nil {
		return Calibration{}, errors.New("nil token counter")
	}
	if maxCalls <= 0 {
		return Calibration{}, errors.New("maxCalls must be positive")
	}
	var totalBytes, totalTokens, used int
	for _, s := range samples {
		if used >= maxCalls {
			break // bound the paid calls
		}
		if s == "" {
			continue
		}
		tok, err := counter.CountTokens(ctx, s)
		if err != nil {
			return Calibration{}, fmt.Errorf("count_tokens (sample %d): %w", used+1, err)
		}
		if tok <= 0 {
			continue // a sample that counts to zero tokens carries no ratio
		}
		totalBytes += len(s)
		totalTokens += tok
		used++
	}
	if used == 0 || totalTokens == 0 {
		return Calibration{}, errors.New("no usable samples to calibrate from — enable savings_samples and run a few wd check/commit actions first")
	}
	return Calibration{
		BytesPerToken: float64(totalBytes) / float64(totalTokens),
		Samples:       used,
		SampleBytes:   totalBytes,
		SampleTokens:  totalTokens,
		Model:         CalibrationModel,
		UpdatedAt:     time.Now().UTC(),
	}, nil
}

// LoadCalibration reads the persisted factor from dir. The bool is false (no
// error) when no calibration file exists yet — an uncalibrated install, which
// reads as "heuristic" rather than a failure — or when the stored factor is
// non-positive (treated as absent rather than poisoning estimation).
func LoadCalibration(dir string) (Calibration, bool, error) {
	b, err := os.ReadFile(filepath.Join(dir, calibrationFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Calibration{}, false, nil
		}
		return Calibration{}, false, err
	}
	var c Calibration
	if err := json.Unmarshal(b, &c); err != nil {
		return Calibration{}, false, err
	}
	if c.BytesPerToken <= 0 {
		return Calibration{}, false, nil
	}
	return c, true, nil
}

// SaveCalibration writes the factor to dir atomically (temp file + rename) so a
// concurrent daemon read never observes a half-written file. Creates dir if needed.
func SaveCalibration(dir string, c Calibration) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, calibrationFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, calibrationFile))
}

// anthropicCounter counts tokens via Claude's count_tokens endpoint. The API key
// is read from ANTHROPIC_API_KEY by the SDK and is never logged or printed.
type anthropicCounter struct {
	client anthropic.Client
	model  anthropic.Model
}

// NewAnthropicCounter builds a count_tokens-backed TokenCounter. It returns an
// error WITHOUT touching any persisted factor when ANTHROPIC_API_KEY is unset, so
// `wd savings --calibrate` can exit non-zero with a clear instruction and the
// heuristic stays in force; every other command never constructs one and runs
// fully offline.
func NewAnthropicCounter() (TokenCounter, error) {
	if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) == "" {
		return nil, errors.New("ANTHROPIC_API_KEY is not set — export it and re-run `wd savings --calibrate` (only --calibrate reaches the network; every other command works offline with the heuristic)")
	}
	// The SDK reads ANTHROPIC_API_KEY itself; we don't pass or echo the key.
	c := anthropic.NewClient()
	return &anthropicCounter{client: c, model: anthropic.ModelClaudeOpus4_8}, nil
}

// CountTokens counts the tokens in text via POST /v1/messages/count_tokens.
func (a *anthropicCounter) CountTokens(ctx context.Context, text string) (int, error) {
	resp, err := a.client.Messages.CountTokens(ctx, anthropic.MessageCountTokensParams{
		Model:    a.model,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(text))},
	})
	if err != nil {
		return 0, err
	}
	return int(resp.InputTokens), nil
}
