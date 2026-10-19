package ptyhost

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/ptyruntime"
	"github.com/srjn45/warden/internal/terminalcontract"
)

// F-PAR-01: golden byte stream: tmux path vs direct path produce identical
// visible output. Asserts budget ParityByteExactPct = 100.
func TestF_PAR_01_GoldenByteStreamParityTmuxVsDirect(t *testing.T) {
	noLeaks(t)
	run := newTmux(t)
	dir := filepath.Join(t.TempDir(), "pty")
	m := New(Config{
		Mode: ModeOn, Dir: dir, Run: run, Shell: []string{"/bin/sh"},
		ReconcileEvery: 50 * time.Millisecond,
		Options:        ptyruntime.Options{IdleFlush: 20 * time.Millisecond, KillGrace: 500 * time.Millisecond, DrainGrace: 300 * time.Millisecond},
	})
	defer m.Close()

	ctx := context.Background()

	// 1. Launch a tmux shell with no prompt. The normal interactive shell prompt
	// redraws asynchronously and can land in the middle of the golden output;
	// this fixture compares terminal rendering, not prompt rendering.
	const tmuxName = "term-tmux"
	m.SetSessionRuntimePin(tmuxName, ModeOff)
	tmuxCWD := t.TempDir()
	_, err := run.Run(ctx, tmuxCWD, "tmux", "new-session", "-d", "-s", tmuxName, "-c", tmuxCWD,
		"/bin/sh", "-c", `while IFS= read -r line; do eval "$line"; done`)
	require.NoError(t, err)
	defer func() { _ = m.Host().KillSession(ctx, tmuxName) }()

	// 2. Launch direct terminal session (via TerminalHost in ModeOn)
	const directName = "term-direct"
	require.NoError(t, m.TerminalHost().NewSession(ctx, directName, t.TempDir()))
	defer func() { _ = m.TerminalHost().KillSession(ctx, directName) }()

	require.Nil(t, m.Direct(tmuxName), "tmuxName must run in tmux")
	require.NotNil(t, m.Direct(directName), "directName must run directly on PTY")

	// Wait for shell readiness on both sessions: require both the typed command and its output.
	for _, name := range []string{tmuxName, directName} {
		require.Eventually(t, func() bool {
			_ = m.Host().SendKeys(ctx, name, "echo READY", "Enter")
			pane, err := m.Host().CapturePane(ctx, name)
			return err == nil && strings.Count(pane, "READY") >= 2
		}, 5*time.Second, 50*time.Millisecond)
	}

	// 3. Send identical golden byte stream commands to both sessions:
	// - simple strings
	// - multiline outputs
	// - tab separations
	// - carriage return overwrites
	// - ANSI colors and formatting
	commands := []string{
		"printf 'Hello Parity Test\\n'",
		"printf 'Line 1\\nLine 2\\nLine 3\\n'",
		"printf 'alpha\\tbeta\\tgamma\\n'",
		"printf 'OldText\\rNewText\\n'",
		"printf '\\033[32mGREEN\\033[0m \\033[1mBOLD\\033[0m\\n'",
		"printf 'PARITY_%s\\n' 'MARKER_DONE'",
	}
	expected := []string{
		"Hello Parity Test",
		"Line 1",
		"Line 2",
		"Line 3",
		"alpha   beta    gamma",
		"NewText",
		"GREEN BOLD",
		"PARITY_MARKER_DONE",
	}

	for _, cmd := range commands {
		require.NoError(t, m.Host().SendKeys(ctx, tmuxName, cmd, "Enter"))
		require.NoError(t, m.Host().SendKeys(ctx, directName, cmd, "Enter"))
	}

	expandTabs := func(s string) string {
		var b strings.Builder
		col := 0
		for _, r := range s {
			if r == '\t' {
				spaces := 8 - (col % 8)
				for i := 0; i < spaces; i++ {
					b.WriteByte(' ')
				}
				col += spaces
			} else {
				b.WriteRune(r)
				col++
			}
		}
		return b.String()
	}

	// Clean trailing empty lines from both screens to compare visible screen content.
	cleanLines := func(s string) []string {
		lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
		// filter only the output lines containing our test tokens
		var filtered []string
		for _, l := range lines {
			trimmed := expandTabs(strings.TrimRight(l, " \t"))
			if strings.Contains(trimmed, "printf ") {
				continue
			}
			if strings.Contains(trimmed, "Hello Parity Test") ||
				strings.Contains(trimmed, "Line 1") ||
				strings.Contains(trimmed, "Line 2") ||
				strings.Contains(trimmed, "Line 3") ||
				strings.Contains(trimmed, "alpha") ||
				strings.Contains(trimmed, "NewText") ||
				strings.Contains(trimmed, "GREEN") ||
				strings.Contains(trimmed, "PARITY_MARKER_DONE") {
				filtered = append(filtered, trimmed)
			}
		}
		return filtered
	}

	var paneTmux, paneDirect string
	var tmuxFiltered, directFiltered []string
	// 4. Wait for a stable, complete screen in both panes. In particular, a
	// tmux capture during prompt redraw can contain the same eight markers with
	// transient "$ " prefixes or a partially-overwritten "NewTextxt". Those
	// are not the completed golden stream and must be retried, not compared.
	require.Eventually(t, func() bool {
		var err1, err2 error
		paneTmux, err1 = m.Host().CapturePane(ctx, tmuxName)
		paneDirect, err2 = m.Host().CapturePane(ctx, directName)
		if err1 != nil || err2 != nil {
			return false
		}
		tmuxFiltered = cleanLines(paneTmux)
		directFiltered = cleanLines(paneDirect)
		return stableParityCapture(tmuxFiltered, expected) && stableParityCapture(directFiltered, expected)
	}, waitFor, 20*time.Millisecond)

	require.NotEmpty(t, tmuxFiltered)
	require.NotEmpty(t, directFiltered)
	require.Equal(t, len(tmuxFiltered), len(directFiltered), "number of matching output lines must be equal")

	// Byte exact parity check across all matching lines
	matchingBytes := 0
	totalBytes := 0
	for i := range tmuxFiltered {
		tLine := tmuxFiltered[i]
		dLine := directFiltered[i]
		maxLen := len(tLine)
		if len(dLine) > maxLen {
			maxLen = len(dLine)
		}
		totalBytes += maxLen
		for j := 0; j < len(tLine) && j < len(dLine); j++ {
			if tLine[j] == dLine[j] {
				matchingBytes++
			}
		}
	}

	var parityPct float64
	if totalBytes > 0 {
		parityPct = float64(matchingBytes) / float64(totalBytes) * 100.0
	}

	budget := terminalcontract.Default.ParityByteExactPct
	require.GreaterOrEqual(t, parityPct, budget, "visible output parity must meet budget ParityByteExactPct (100%%)")
	require.Equal(t, tmuxFiltered, directFiltered, "golden byte stream output lines must match 100%% byte-exact")
}

func stableParityCapture(got, want []string) bool {
	return slices.Equal(got, want)
}

func TestStableParityCaptureRejectsPromptRedrawContamination(t *testing.T) {
	want := []string{"Hello Parity Test", "NewText", "PARITY_MARKER_DONE"}
	if stableParityCapture([]string{"$ Hello Parity Test", "NewTextxt", "$ PARITY_MARKER_DONE"}, want) {
		t.Fatal("prompt/redraw-contaminated tmux capture was accepted as stable")
	}
	if !stableParityCapture(append([]string(nil), want...), want) {
		t.Fatal("completed golden capture was not accepted")
	}
}
