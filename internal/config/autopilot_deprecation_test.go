package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestDeprecatedAutopilotBackendsStillParse proves the deprecated keys keep loading
// (so the one-time store migration can read them) while emitting a warning.
func TestDeprecatedAutopilotBackendsStillParse(t *testing.T) {
	body := "" +
		"autopilot:\n" +
		"  brain:\n" +
		"    backends:\n" +
		"      free: [antigravity]\n" +
		"      subscription: [claude]\n" +
		"      pay_per_use: [gpt]\n" +
		"    allow_pay_per_use: true\n"
	path := tmpConfig(t, body)

	// Capture slog so we can assert the deprecation warnings fire on load.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	c := Load(path)

	// Parsing is preserved — the migration reads the ladder + gate through the
	// (deprecated) accessors.
	require.Equal(t, []string{"antigravity"}, c.AutopilotBrainBackends().Free)
	require.Equal(t, []string{"claude"}, c.AutopilotBrainBackends().Subscription)
	require.Equal(t, []string{"gpt"}, c.AutopilotBrainBackends().PayPerUse)
	require.True(t, c.AutopilotAllowPayPerUse())

	out := buf.String()
	require.Contains(t, out, "autopilot.brain.backends")
	require.Contains(t, out, "autopilot.brain.allow_pay_per_use")
}

// TestWarnDeprecatedAutopilotBackendsSilentWhenAbsent proves the warning does not
// fire for a config that never mentions the deprecated keys.
func TestWarnDeprecatedAutopilotBackendsSilentWhenAbsent(t *testing.T) {
	var doc yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte("addr: localhost:8765\nautopilot:\n  enabled: false\n"), &doc))

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	warnDeprecatedAutopilotBackends(doc.Content[0])
	require.False(t, strings.Contains(buf.String(), "deprecated key autopilot.brain"))
}
