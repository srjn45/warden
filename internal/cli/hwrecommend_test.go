package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/config"
)

func TestRecommendModel(t *testing.T) {
	cases := []struct {
		gb   float64
		want string
	}{
		{40, "qwen2.5-coder:32b"},
		{24, "qwen2.5-coder:32b"},
		{16, "qwen2.5-coder:14b"},
		{10, "qwen2.5-coder:14b"},
		{8, "qwen2.5-coder:7b"},
		{6, "qwen2.5-coder:7b"},
		{4, "qwen2.5-coder:3b"},
		{3.5, "qwen2.5-coder:3b"},
		{2, "qwen2.5-coder:1.5b"},
		{1, "qwen2.5-coder:1.5b"},
		{0, "qwen2.5-coder:1.5b"},
	}
	for _, c := range cases {
		require.Equal(t, c.want, recommendModel(c.gb), "gb=%v", c.gb)
	}
}

func TestDetectMemoryGB_NvidiaWins(t *testing.T) {
	// Two GPUs reported (MiB); the larger one is chosen.
	run := func(name string, args ...string) ([]byte, error) {
		if name == "nvidia-smi" {
			return []byte("8192\n24576\n"), nil
		}
		return nil, errors.New("not called")
	}
	m := detectMemoryGB(run, "linux", func() (float64, bool) { return 64, true })
	require.True(t, m.ok)
	require.InDelta(t, 24, m.gb, 0.01)
	require.Contains(t, m.source, "VRAM")
}

func TestDetectMemoryGB_AppleUnified(t *testing.T) {
	run := func(name string, args ...string) ([]byte, error) {
		if name == "sysctl" {
			return []byte("17179869184\n"), nil // 16 GiB
		}
		return nil, errors.New("no nvidia")
	}
	m := detectMemoryGB(run, "darwin", func() (float64, bool) { return 0, false })
	require.True(t, m.ok)
	require.InDelta(t, 16, m.gb, 0.01)
	require.Contains(t, m.source, "unified")
}

func TestDetectMemoryGB_FallsBackToSystemRAM(t *testing.T) {
	run := func(string, ...string) ([]byte, error) { return nil, errors.New("nothing") }
	m := detectMemoryGB(run, "linux", func() (float64, bool) { return 4, true })
	require.True(t, m.ok)
	require.InDelta(t, 4, m.gb, 0.01)
	require.Contains(t, m.source, "system RAM")
}

func TestDetectMemoryGB_NothingDetected(t *testing.T) {
	run := func(string, ...string) ([]byte, error) { return nil, errors.New("nothing") }
	m := detectMemoryGB(run, "linux", func() (float64, bool) { return 0, false })
	require.False(t, m.ok)
}

func TestLocalLLMAdvice(t *testing.T) {
	mem := memProbe{gb: 4, source: "system RAM (no GPU detected)", ok: true}

	// local_llm off → tell the operator to enable it.
	off := localLLMAdvice(config.Config{LocalLLM: false}, mem)
	require.True(t, off.ok)
	require.False(t, off.required, "the recommendation is advisory, never a failure")
	require.Contains(t, off.detail, "qwen2.5-coder:3b")
	require.Contains(t, off.detail, "local_llm: true")

	// on with a matching model → confirmed.
	match := localLLMAdvice(config.Config{LocalLLM: true, LocalLLMModel: "qwen2.5-coder:3b"}, mem)
	require.Contains(t, match.detail, "qwen2.5-coder:3b")
	require.NotContains(t, match.detail, "change local_llm_model", "no change needed when it already matches")

	// on with a mismatched model → point at the config file, never auto-swap.
	miss := localLLMAdvice(config.Config{LocalLLM: true, LocalLLMModel: "llama3:8b"}, mem)
	require.Contains(t, miss.detail, "llama3:8b")
	require.Contains(t, miss.detail, "change local_llm_model to qwen2.5-coder:3b in your config file")

	// detection failure → conservative floor.
	undetected := localLLMAdvice(config.Config{LocalLLM: true}, memProbe{ok: false})
	require.Contains(t, undetected.detail, "qwen2.5-coder:1.5b")
}

func TestFormatReportIncludesAdvisory(t *testing.T) {
	// An advisory checkResult prints as informational and never flips the verdict.
	results := []checkResult{
		{name: "tmux", ok: true, required: true, detail: "/usr/bin/tmux"},
		{name: "local llm", ok: true, required: false, detail: "detected ~4 GB system RAM → recommend qwen2.5-coder:3b"},
	}
	out := formatReport("dev", results)
	require.Contains(t, out, "local llm")
	require.Contains(t, out, "all required checks passed")
	require.True(t, strings.Contains(out, "qwen2.5-coder:3b"))
}
