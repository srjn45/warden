package tui

import (
	"fmt"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// itemFor returns the first list item matching pred, or a zero item.
func itemFor(m controlPaneModel, pred func(item) bool) item {
	for _, it := range m.items() {
		if pred(it) {
			return it
		}
	}
	return item{}
}

func onAgentID(id string) func(item) bool {
	return func(it item) bool { return it.session != nil && !it.session.IsTerminal() && it.session.ID == id }
}

func onSessionID(id string) func(item) bool {
	return func(it item) bool { return it.session != nil && it.session.ID == id }
}

// items() flags the openedAgent and openedTerminal rows so the control pane can
// mark what is docked in each pane; unrelated rows stay unflagged.
func TestItemsMarkOpenedAgentAndTerminal(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "%1")
	m.defaultTerminalReady = true
	m.sessions = []*store.Session{
		liveAgent("a1", "/a1"),
		liveAgent("a2", "/a2"),
		liveTerminal("t1", "/w1", time.Now()),
		liveTerminal("t2", "/w2", time.Now().Add(time.Second)),
	}
	m.openedAgent = "a2"
	m.openedTerminal = "t1"

	require.True(t, itemFor(m, onAgentID("a2")).opened, "the opened agent row is flagged")
	require.False(t, itemFor(m, onAgentID("a1")).opened, "a non-opened agent is not flagged")
	require.True(t, itemFor(m, onSessionID("t1")).opened, "the opened terminal row is flagged")
	require.False(t, itemFor(m, onSessionID("t2")).opened, "a non-opened terminal is not flagged")
}

// A pipeline agent shown in the agent pane flags its Pipelines job row (matched on
// the linked live session), so the marker shows where that agent actually renders.
func TestItemsMarkOpenedPipelineJob(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "%1")
	m.defaultTerminalReady = true
	pa := liveAgent("pa1", "/p/a1")
	pa.PipelineID = "demo" // owned by the pipeline → renders only as a job row
	m.sessions = []*store.Session{pa}
	m.pipelines = []*pipeline.Pipeline{{ID: "demo", Name: "demo", Status: pipeline.StatusRunning, Jobs: []pipeline.Job{
		{ID: "j1", SessionID: "pa1", Status: pipeline.JobRunning},
	}}}
	m.openedAgent = "pa1"

	require.True(t, itemFor(m, onJob("j1")).opened, "the opened pipeline agent's job row is flagged")
}

// renderItemLine marks the opened (but not cursor-selected) row with the ◆ gutter;
// a plain row has none, and the cursor caret wins when it sits on the opened row.
func TestOpenedMarkerRendering(t *testing.T) {
	opened := item{session: liveAgent("a1", "/a1"), opened: true}
	require.Contains(t, renderItemLine(opened, false, 80), "◆", "an opened, unselected row shows the ◆ marker")
	require.NotContains(t, renderItemLine(item{session: liveAgent("a2", "/a2")}, false, 80), "◆", "a plain row has no marker")

	both := renderItemLine(opened, true, 80)
	require.Contains(t, both, "›", "the cursor caret shows when the cursor is on the opened row")
	require.NotContains(t, both, "◆", "the cursor gutter wins over the opened marker")
}

// The opened agent's name renders as a bold magenta badge (stOpenedName) so it is
// unmistakable; a non-opened row's name is plain, and the cursor row keeps the
// name unstyled so the whole-row cursor highlight reaches through it.
func TestOpenedNameBadge(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	s := &store.Session{ID: "a1", Name: "builder", Status: store.StatusWorking}
	badge := stOpenedName.Render(fmt.Sprintf("%-16s", "builder"))

	require.Contains(t, renderItemLine(item{session: s, opened: true}, false, 80), badge,
		"the opened agent's name is a bold magenta badge")
	require.NotContains(t, renderItemLine(item{session: s}, false, 80), badge,
		"a non-opened agent's name is plain")
	require.NotContains(t, renderItemLine(item{session: s, opened: true}, true, 80), badge,
		"the cursor row leaves the name unstyled for the whole-row highlight")
}

// The opened highlight follows §8 M-a rotation: after Alt+a the marker moves to
// the newly-docked agent even though the control-pane cursor has not moved.
func TestOpenedMarkerFollowsRotation(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "%1")
	m.defaultTerminalReady = true
	m.sessions = []*store.Session{liveAgent("a1", "/a1"), liveAgent("a2", "/a2")}
	m.openedAgent = "a1"
	require.True(t, itemFor(m, onAgentID("a1")).opened)

	m = lstep(m, altKey('a'))
	require.Equal(t, "a2", m.openedAgent)
	require.True(t, itemFor(m, onAgentID("a2")).opened, "the marker follows rotation to a2")
	require.False(t, itemFor(m, onAgentID("a1")).opened, "a1 is no longer marked opened")
}
