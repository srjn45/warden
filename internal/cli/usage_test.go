package cli

import (
	"bytes"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/stretchr/testify/require"
)

func TestPrintUsageMultipleWindowsAndUnknowns(t *testing.T) {
	reset := time.Date(2026, 9, 7, 10, 48, 0, 0, time.UTC)
	duration := 10080
	used := 62.0
	remaining := 38.0
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := printUsage(cmd, backendusage.Snapshot{Backends: []backendusage.BackendResult{{ID: "claude", Status: backendusage.StatusUnsupported, Windows: []backendusage.Window{}, Error: &backendusage.ProviderError{Message: "no structured usage"}}, {ID: "codex", Status: backendusage.StatusOK, Account: &backendusage.Account{Plan: "plus", Label: "secret@example.invalid"}, Windows: []backendusage.Window{{ID: "codex:secondary", UsedPercent: &used, RemainingPercent: &remaining, DurationMinutes: &duration, ResetsAt: &reset}}}}})
	require.NoError(t, err)
	require.Contains(t, out.String(), "codex")
	require.Contains(t, out.String(), "1w")
	require.Contains(t, out.String(), "62%")
	require.Contains(t, out.String(), "claude: no structured usage")
	require.NotContains(t, out.String(), "secret@example.invalid")
}

func TestUsageExitCode(t *testing.T) {
	require.Equal(t, 2, ExitCode(partialResultError{}))
	require.Equal(t, 1, ExitCode(assertionError{}))
}

type assertionError struct{}

func (assertionError) Error() string { return "ordinary" }
