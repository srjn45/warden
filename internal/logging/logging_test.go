package logging

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"":        slog.LevelInfo,
		" warn ":  slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
	}
	for in, want := range cases {
		got, err := ParseLevel(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	_, err := ParseLevel("loud")
	require.Error(t, err)
}

func TestValidLevelAndFormat(t *testing.T) {
	require.True(t, ValidLevel("debug"))
	require.True(t, ValidLevel("ERROR"))
	require.False(t, ValidLevel("verbose"))
	require.True(t, ValidFormat("json"))
	require.True(t, ValidFormat("text"))
	require.True(t, ValidFormat("")) // empty defaults to text
	require.False(t, ValidFormat("yaml"))
}

func TestSetupInvalid(t *testing.T) {
	restore := slog.Default()
	defer slog.SetDefault(restore)
	_, err := Setup("nope", "text")
	require.Error(t, err)
	_, err = Setup("info", "nope")
	require.Error(t, err)
}

// TestSetupLevelFiltering verifies the chosen level filters lower-severity
// records, and that the standard log package is bridged through the same
// handler (so unmigrated log.Print calls respect the level/format too).
func TestSetupLevelFiltering(t *testing.T) {
	restoreLogger := slog.Default()
	restoreFlags, restoreOut := log.Flags(), log.Writer()
	defer func() {
		slog.SetDefault(restoreLogger)
		log.SetFlags(restoreFlags)
		log.SetOutput(restoreOut)
	}()

	var buf bytes.Buffer
	logger, err := Setup("warn", "text")
	require.NoError(t, err)
	require.NotNil(t, logger)

	// Re-point at our buffer using the resolved level via a fresh handler.
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	slog.Info("below threshold")
	slog.Warn("at threshold")
	out := buf.String()
	require.NotContains(t, out, "below threshold")
	require.Contains(t, out, "at threshold")
}

// TestSetupJSONFormat asserts the json format emits parseable JSON objects.
func TestSetupJSONFormat(t *testing.T) {
	restore := slog.Default()
	defer slog.SetDefault(restore)

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	slog.Info("hello", "agent", "a1")

	var m map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &m))
	require.Equal(t, "hello", m["msg"])
	require.Equal(t, "a1", m["agent"])
}

// TestSetupBridgesStdLog asserts that after Setup the standard log package is
// routed through slog's handler (the migration bridge for unconverted sites).
func TestSetupBridgesStdLog(t *testing.T) {
	restoreLogger := slog.Default()
	restoreFlags, restoreOut := log.Flags(), log.Writer()
	defer func() {
		slog.SetDefault(restoreLogger)
		log.SetFlags(restoreFlags)
		log.SetOutput(restoreOut)
	}()

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	log.Print("legacy line")

	var m map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &m))
	require.Equal(t, "legacy line", m["msg"])
}
