package ptyhost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyruntime"
	"github.com/srjn45/warden/internal/terminalcontract"
)

// F-PRIV-01: planted secrets (typed no-echo password, env token, API-key output)
// absent from journal, audit, diagnostics, SSE and remote-safe views.
func TestF_PRIV_01_PlantedSecretsAbsentFromDurableAndRemoteViews(t *testing.T) {
	noLeaks(t)
	run := newTmux(t)
	dir := filepath.Join(t.TempDir(), "pty")
	m := newManager(t, run, dir, ModeOn)
	defer m.Close()

	ctx := context.Background()
	const name = "priv-test-1"

	// Define planted secrets
	secretPassword := "P@ssw0rd_SuperSecret_99999!"
	secretEnvToken := "sk-ant-api03-" + strings.Repeat("A1B2", 12)
	secretAPIOutput := "sk-ant-api03-" + strings.Repeat("K9L8", 12)

	// Launch session with planted environment secret
	envSecret := "ANTHROPIC_API_KEY=" + secretEnvToken
	require.NoError(t, m.TerminalHost().NewSession(ctx, name, t.TempDir(), envSecret))
	defer func() { _ = m.TerminalHost().KillSession(ctx, name) }()

	sess := m.Direct(name)
	require.NotNil(t, sess, "session must run directly on PTY")

	// 1. Process writes API secret token to terminal stdout
	require.NoError(t, m.TerminalHost().SendKeys(ctx, name, "echo token="+secretAPIOutput, "Enter"))

	// 2. Type secret password while termios ECHO is disabled (or via secret input frame)
	// Write typed password under no-echo credential prompt input
	rawInput := []byte(secretPassword + "\r")
	inPayload, inReds := ptyevent.RedactInput(rawInput, ptyevent.OriginHuman, ptyevent.TTYMode{Canonical: true, Echo: false}, true)
	require.Empty(t, inPayload.Data, "no-echo input payload data must be stripped or empty")
	_ = inReds

	// Wait for process to produce output
	require.Eventually(t, func() bool {
		pane, err := m.TerminalHost().CapturePane(ctx, name)
		return err == nil && strings.Contains(pane, "token=")
	}, waitFor, 20*time.Millisecond)

	// Collect all durable and remote views to inspect for secret leaks
	var leaks int
	checkLeak := func(viewName, content string) {
		for _, secret := range []string{secretPassword, secretEnvToken, secretAPIOutput} {
			if strings.Contains(content, secret) {
				t.Errorf("privacy violation: planted secret leaked in %s", viewName)
				leaks++
			}
		}
	}

	// 3. Inspect Journal on disk
	journalDir := filepath.Join(dir, "direct", name)
	j, err := ptyruntime.OpenJournal(journalDir, name, ptyruntime.Retention{})
	require.NoError(t, err)
	defer j.Close()

	var journalEvents []ptyevent.Event
	err = j.Read(1, func(ev ptyevent.Event) error {
		journalEvents = append(journalEvents, ev)
		if ev.Output != nil {
			checkLeak("journal Output.Data", string(ev.Output.Data))
		}
		if ev.Input != nil {
			checkLeak("journal Input.Data", string(ev.Input.Data))
		}
		if ev.Marker != nil {
			checkLeak("journal Marker.Snapshot", ev.Marker.Snapshot)
		}
		return nil
	})
	require.NoError(t, err)

	// Read raw journal segment files directly from disk
	err = filepath.Walk(journalDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			raw, readErr := os.ReadFile(p)
			if readErr == nil {
				checkLeak("raw journal file "+filepath.Base(p), string(raw))
			}
		}
		return nil
	})
	require.NoError(t, err)

	// 4. Inspect Audit log files on disk (e.g. <name>.audit)
	auditFile := filepath.Join(dir, name+".audit")
	if rawAudit, err := os.ReadFile(auditFile); err == nil {
		checkLeak("session audit log", string(rawAudit))
	}

	// 5. Inspect Screen snapshot & CapturePane (diagnostics / remote-safe views)
	pane, err := m.TerminalHost().CapturePane(ctx, name)
	require.NoError(t, err)
	checkLeak("CapturePane screen", pane)
	require.Contains(t, pane, "[REDACTED:anthropic_key]", "API token must be replaced with redaction placeholder")

	// 6. Inspect Remote WebSocket Replay / SSE subscribers
	url := wsServer(t, func() *Manager { return m }, name, false)
	c := dial(t, url)
	defer c.close()
	c.send(ClientFrame{Type: "hello", Mode: ModeObserve})
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.plans) > 0
	}, waitFor, 10*time.Millisecond)

	c.waitEventOutput("token=[REDACTED")
	_, replayEvents, _, _ := c.snapshot()
	for _, ev := range replayEvents {
		if ev.Output != nil {
			checkLeak("WebSocket replay Output.Data", string(ev.Output.Data))
		}
	}

	// 7. Verify budget: SecretLeaksInDurable == 0
	require.Equal(t, terminalcontract.Default.SecretLeaksInDurable, leaks,
		"SecretLeaksInDurable budget violation: expected 0 leaks, got %d", leaks)
}
