package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srjn45/warden/internal/fastbrain"
)

// Seams so tests never touch the user's home or a real gh.
var (
	tuiCrashDir = fastbrain.DefaultCrashDir
	tuiBugGH    = fastbrain.DefaultGH
)

// bugScanInterval throttles the local draft-directory scan (the tick is 1s).
const bugScanInterval = 5 * time.Second

// bugBadgeText is the status-bar chip shown while a staged warden-bug draft awaits review.
const bugBadgeText = "[⚠️ Bug Detected: Press B to Review]"

type bugDraftsMsg struct{ drafts []*fastbrain.IssueDraft }

type bugSubmitMsg struct {
	id    string
	link  string
	viaGH bool
	err   error
}

// bugDraftsCmd lists staged drafts straight from disk — no daemon endpoint.
func bugDraftsCmd() tea.Cmd {
	return func() tea.Msg {
		dir, err := tuiCrashDir()
		if err != nil {
			return bugDraftsMsg{}
		}
		d, _ := fastbrain.ListCrashDrafts(dir)
		return bugDraftsMsg{drafts: d}
	}
}

// bugSubmitCmd runs only after the user pressed Submit.
func bugSubmitCmd(d *fastbrain.IssueDraft) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		link, viaGH, err := fastbrain.SubmitBugReport(ctx, tuiBugGH(), d)
		return bugSubmitMsg{id: d.ID, link: link, viaGH: viaGH, err: err}
	}
}

// pendingBugDraft is the newest staged, not-dismissed draft, or nil.
func (m controlPaneModel) pendingBugDraft() *fastbrain.IssueDraft {
	for _, d := range m.bugDrafts {
		if d.Status == "staged" && !m.bugDismissed[d.ID] {
			return d
		}
	}
	return nil
}

func (m controlPaneModel) bugReviewBody() string {
	d := m.pendingBugDraft()
	if d == nil {
		return "no staged bug report"
	}
	return fmt.Sprintf("Title:\n  %s\n\nEnvironment:\n  %s\n\nSanitized stack:\n%s\n\nNothing is uploaded unless you choose Submit.",
		d.Title, d.Environment, d.Stack)
}

func (m controlPaneModel) handleBugReviewKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "s", "S":
		d := m.pendingBugDraft()
		m.mode = modeNormal
		if d == nil {
			return m, nil
		}
		m.status = "submitting bug report…"
		return m, bugSubmitCmd(d)
	case "d", "D", "esc", "n", "N", "q":
		if d := m.pendingBugDraft(); d != nil {
			if m.bugDismissed == nil {
				m.bugDismissed = map[string]bool{}
			}
			m.bugDismissed[d.ID] = true
		}
		m.mode = modeNormal
		m.status = "bug report dismissed (draft stays staged; warden bug-report <id> to revisit)"
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func bugSubmitStatus(msg bugSubmitMsg) string {
	switch {
	case msg.err != nil:
		return "bug report failed: " + msg.err.Error()
	case msg.viaGH:
		return "bug report filed: " + msg.link
	default:
		return "gh not authenticated — open: " + strings.TrimSpace(msg.link)
	}
}
