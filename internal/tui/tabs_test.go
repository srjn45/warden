package tui

import (
	"testing"
	"time"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// tabBarTitle brackets the active tab and joins the tabs with border dashes, so
// titleBox splices it into the top border as `╭─ Projects ─[ Terminals ]─╮`.
func TestTabBarTitle(t *testing.T) {
	require.Equal(t, "[ Projects ]─ Terminals", tabBarTitle(tabProjects))
	require.Equal(t, "Projects ─[ Terminals ]", tabBarTitle(tabTerminals))
}

// The Tab key cycles the active tab (Projects → Terminals → Projects) and the
// navigator's items() follows: agents show only on Projects, terminals only on
// Terminals.
func TestTabKeyCyclesAndFilters(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "%1")
	m.defaultTerminalReady = true // suppress the startup ensure so it doesn't spawn
	m = lstep(m, sessionsMsg{sessions: []*store.Session{
		liveAgent("a1", "/a1"),
		liveTerminal("t1", "/w1", time.Now()),
	}})

	// Fresh cockpit opens on Projects: the agent is listed, the terminal is not.
	require.Equal(t, tabProjects, m.currentTab)
	require.Contains(t, itemSessionIDs(m.items()), "a1", "agent shows on the Projects tab")
	require.NotContains(t, itemSessionIDs(m.items()), "t1", "terminal hidden on the Projects tab")

	// Tab → Terminals: only the terminal is listed now.
	m = lstep(m, key("tab"))
	require.Equal(t, tabTerminals, m.currentTab)
	require.Contains(t, itemSessionIDs(m.items()), "t1", "terminal shows on the Terminals tab")
	require.NotContains(t, itemSessionIDs(m.items()), "a1", "agent hidden on the Terminals tab")

	// Tab again wraps back to Projects.
	m = lstep(m, key("tab"))
	require.Equal(t, tabProjects, m.currentTab, "Tab wraps back to Projects")
}

// Cycling to an empty tab re-pins the cursor to a valid row (the section header)
// rather than leaving it dangling past the end of the shorter list.
func TestTabKeyRepinsCursor(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "%1")
	m.defaultTerminalReady = true
	// Several agents so the Projects cursor can sit well past index 0, and no
	// terminals so the Terminals tab is just its (empty) section header.
	m = lstep(m, sessionsMsg{sessions: []*store.Session{
		liveAgent("a1", "/a1"), liveAgent("a2", "/a2"), liveAgent("a3", "/a3"),
	}})
	m.cursor = len(m.items()) - 1 // land on the last Projects row

	m = lstep(m, key("tab")) // → Terminals (only a header row)
	require.Less(t, m.cursor, len(m.items()), "cursor stays within the shorter Terminals list")
	require.GreaterOrEqual(t, m.cursor, 0, "cursor stays non-negative")
}
