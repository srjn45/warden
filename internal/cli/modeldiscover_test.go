package cli

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseCursorDiscover(t *testing.T) {
	in := []byte(`Available models

auto - Auto (current, default)
gpt-5.3-codex-low - Codex 5.3 Low
glm-5.2-max - GLM 5.2 Max

Tip: use --model <id> (or /model <id> …) to switch.
`)
	got := parseCursorDiscover(in, "cursor")
	require.Equal(t, []DiscoveredModel{
		{BackendID: "cursor", ModelID: "auto", DisplayName: "Auto"},
		{BackendID: "cursor", ModelID: "gpt-5.3-codex-low", DisplayName: "Codex 5.3 Low"},
		{BackendID: "cursor", ModelID: "glm-5.2-max", DisplayName: "GLM 5.2 Max"},
	}, got)
	require.Empty(t, parseCursorDiscover(nil, "cursor"))
}

func TestParseAgyDiscover(t *testing.T) {
	in := []byte(`Fetching available models...
gemini-3.8-flash-high	Gemini 3.8 Flash (High)
gemini-3.7-flash-low	Gemini 3.7 Flash (Low)

legacy-display-only
`)
	got := parseAgyDiscover(in, "antigravity")
	require.Equal(t, []DiscoveredModel{
		{BackendID: "antigravity", ModelID: "gemini-3.8-flash-high", DisplayName: "Gemini 3.8 Flash (High)"},
		{BackendID: "antigravity", ModelID: "gemini-3.7-flash-low", DisplayName: "Gemini 3.7 Flash (Low)"},
		{BackendID: "antigravity", ModelID: "legacy-display-only", DisplayName: "legacy-display-only"},
	}, got)
}

func TestParseOneIDPerLine(t *testing.T) {
	in := []byte(`opencode/big-pickle
ollama/qwen2.5-coder:3b

# comment
aihubmix/auto (not configured)
crush/gpt-5
`)
	got := parseOneIDPerLine(in, "opencode")
	require.Equal(t, []DiscoveredModel{
		{BackendID: "opencode", ModelID: "opencode/big-pickle", DisplayName: "opencode/big-pickle"},
		{BackendID: "opencode", ModelID: "ollama/qwen2.5-coder:3b", DisplayName: "ollama/qwen2.5-coder:3b"},
		{BackendID: "opencode", ModelID: "crush/gpt-5", DisplayName: "crush/gpt-5"},
	}, got)
}

func TestParseCodexConfigModels(t *testing.T) {
	in := []byte(`personality = "pragmatic"
model = "gpt-6.1-sol"
model_reasoning_effort = "low"

[projects."/home/user/repo"]
trust_level = "trusted"

model = "ignored-nested"
`)
	got := parseCodexConfigModels(in, "codex")
	require.Equal(t, []DiscoveredModel{
		{BackendID: "codex", ModelID: "gpt-6.1-sol", DisplayName: "gpt-6.1-sol"},
	}, got)

	require.Nil(t, parseCodexConfigModels([]byte(`[tui]
model = "too-late"
`), "codex"))
}

func TestDiscoverInstalledModelsUsesStubs(t *testing.T) {
	origCursor, origAgy, origOC, origCrush, origPath, origRead := discoverCursorCmd, discoverAgyCmd, discoverOpencodeCmd, discoverCrushCmd, discoverCodexConfigPath, discoverReadFile
	t.Cleanup(func() {
		discoverCursorCmd, discoverAgyCmd, discoverOpencodeCmd, discoverCrushCmd = origCursor, origAgy, origOC, origCrush
		discoverCodexConfigPath, discoverReadFile = origPath, origRead
	})

	discoverCursorCmd = func() ([]byte, error) {
		return []byte("foo - Foo\n"), nil
	}
	discoverAgyCmd = func() ([]byte, error) {
		return []byte("bar\tBar Display\n"), nil
	}
	discoverOpencodeCmd = func() ([]byte, error) {
		return []byte("opencode/x\n"), nil
	}
	discoverCrushCmd = func() ([]byte, error) {
		return []byte("crush/y\n"), nil
	}
	discoverCodexConfigPath = func() string { return "/tmp/fake-codex.toml" }
	discoverReadFile = func(string) ([]byte, error) {
		return []byte("model = \"gpt-local\"\n"), nil
	}

	all, err := discoverInstalledModels("")
	require.NoError(t, err)
	require.Equal(t, []DiscoveredModel{
		{BackendID: "antigravity", ModelID: "bar", DisplayName: "Bar Display"},
		{BackendID: "codex", ModelID: "gpt-local", DisplayName: "gpt-local"},
		{BackendID: "crush", ModelID: "crush/y", DisplayName: "crush/y"},
		{BackendID: "cursor", ModelID: "foo", DisplayName: "Foo"},
		{BackendID: "opencode", ModelID: "opencode/x", DisplayName: "opencode/x"},
	}, all)

	only, err := discoverInstalledModels("cursor")
	require.NoError(t, err)
	require.Equal(t, []DiscoveredModel{
		{BackendID: "cursor", ModelID: "foo", DisplayName: "Foo"},
	}, only)
}

func TestDiscoverInstalledModelsSoftFailsMissingTools(t *testing.T) {
	origCursor, origAgy, origOC, origCrush, origPath := discoverCursorCmd, discoverAgyCmd, discoverOpencodeCmd, discoverCrushCmd, discoverCodexConfigPath
	t.Cleanup(func() {
		discoverCursorCmd, discoverAgyCmd, discoverOpencodeCmd, discoverCrushCmd = origCursor, origAgy, origOC, origCrush
		discoverCodexConfigPath = origPath
	})
	missing := errors.New("missing")
	discoverCursorCmd = func() ([]byte, error) { return nil, missing }
	discoverAgyCmd = func() ([]byte, error) { return nil, missing }
	discoverOpencodeCmd = func() ([]byte, error) { return nil, missing }
	discoverCrushCmd = func() ([]byte, error) { return nil, missing }
	discoverCodexConfigPath = func() string { return "" }

	got, err := discoverInstalledModels("")
	require.NoError(t, err)
	require.Empty(t, got)
}
