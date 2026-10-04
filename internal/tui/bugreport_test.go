package tui

import (
	"context"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/stretchr/testify/require"
)

type tuiFakeGH struct{ calls int }

func (f *tuiFakeGH) Authenticated(context.Context) bool { return true }
func (f *tuiFakeGH) Run(context.Context, ...string) (string, error) {
	f.calls++
	return "https://github.com/srjn45/warden/issues/7", nil
}

func bugModel(t *testing.T, gh *tuiFakeGH) controlPaneModel {
	dir := t.TempDir()
	_, err := fastbrain.StageCrashDraft(dir, &fastbrain.IssueDraft{
		ID: "a1", CreatedAt: time.Now(), Title: "crash(x): boom", Environment: "Warden v1 (linux/amd64)",
		Stack: "panic: boom", Status: "staged",
	})
	require.NoError(t, err)
	tuiCrashDir = func() (string, error) { return dir, nil }
	tuiBugGH = func() fastbrain.GHRunner { return gh }
	t.Cleanup(func() { tuiCrashDir, tuiBugGH = fastbrain.DefaultCrashDir, fastbrain.DefaultGH })
	m := newListPane(&fakeAPI{}, "", "")
	m.ready = true
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	nm, _ = nm.Update(bugDraftsCmd()())
	return nm.(controlPaneModel)
}

func TestBugBadgeAndModalSubmit(t *testing.T) {
	gh := &tuiFakeGH{}
	m := bugModel(t, gh)
	require.Contains(t, m.View(), bugBadgeText)

	nm, _ := m.Update(key("B"))
	m = nm.(controlPaneModel)
	require.Equal(t, modeBugReview, m.mode)
	require.Contains(t, m.View(), "crash(x): boom")
	require.Zero(t, gh.calls)

	nm, cmd := m.Update(key("s"))
	m = nm.(controlPaneModel)
	require.NotNil(t, cmd)
	nm, _ = m.Update(cmd())
	m = nm.(controlPaneModel)
	require.Equal(t, 1, gh.calls)
	require.Contains(t, m.status, "issues/7")
	require.NotContains(t, m.View(), bugBadgeText)
	require.Nil(t, m.pendingBugDraft())
}

func TestBugModalDismissDoesNotUpload(t *testing.T) {
	gh := &tuiFakeGH{}
	m := bugModel(t, gh)
	nm, _ := m.Update(key("B"))
	nm, cmd := nm.(controlPaneModel).Update(key("d"))
	m = nm.(controlPaneModel)
	require.Nil(t, cmd)
	require.Zero(t, gh.calls)
	require.Equal(t, modeNormal, m.mode)
	require.Nil(t, m.pendingBugDraft())
}

func TestBKeyWithoutDraftAndLowerBUntouched(t *testing.T) {
	m := newListPane(&fakeAPI{}, "", "")
	m.ready = true
	nm, _ := m.Update(key("B"))
	require.Equal(t, modeNormal, nm.(controlPaneModel).mode)
	nm, _ = m.Update(key("b"))
	require.Equal(t, modeBackends, nm.(controlPaneModel).mode)
}
