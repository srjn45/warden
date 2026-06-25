package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWriterAppendsJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w := NewWriter(path)

	w.Log(Event{Action: ActionSpawn, Actor: "127.0.0.1", Target: "abc123", Detail: map[string]string{"name": "fixer"}})
	w.Log(Event{Action: ActionTerminate, Actor: "10.0.0.5", Target: "abc123"})

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	require.Len(t, lines, 2, "one JSON object per line")

	var first Event
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &first))
	require.Equal(t, ActionSpawn, first.Action)
	require.Equal(t, "abc123", first.Target)
	require.Equal(t, "fixer", first.Detail["name"])
	require.False(t, first.Time.IsZero(), "Time is stamped when unset")
}

func TestWriterStampsTimeButKeepsExplicit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w := NewWriter(path)
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	w.Log(Event{Action: ActionApprove, Time: ts})

	events, err := Read(path, Filter{})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.True(t, events[0].Time.Equal(ts), "explicit Time is preserved")
}

func TestWriterFilePerms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	NewWriter(path).Log(Event{Action: ActionSpawn})
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "audit file is owner-only")
}

func TestNilWriterIsNoop(t *testing.T) {
	var w *Writer
	require.NotPanics(t, func() { w.Log(Event{Action: ActionSpawn}) })
}

func TestWriterConcurrentNoInterleave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w := NewWriter(path)

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Log(Event{Action: ActionSpawn, Target: "agent"})
		}()
	}
	wg.Wait()

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	count := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev Event
		require.NoError(t, json.Unmarshal(sc.Bytes(), &ev), "each line is intact, valid JSON")
		count++
	}
	require.NoError(t, sc.Err())
	require.Equal(t, n, count, "every concurrent write produced exactly one line")
}

func TestReadMissingFile(t *testing.T) {
	events, err := Read(filepath.Join(t.TempDir(), "nope.jsonl"), Filter{})
	require.NoError(t, err)
	require.Empty(t, events)
}

func TestReadSkipsMalformedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	good1, _ := json.Marshal(Event{Time: time.Now(), Action: ActionSpawn, Target: "a"})
	good2, _ := json.Marshal(Event{Time: time.Now(), Action: ActionTerminate, Target: "a"})
	// Blank line, a garbage line, and a truncated partial line interleaved.
	content := string(good1) + "\n\n{not json\n" + string(good2) + "\n{\"action\":\"spaw"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	events, err := Read(path, Filter{})
	require.NoError(t, err)
	require.Len(t, events, 2, "only the two well-formed records survive")
	require.Equal(t, ActionSpawn, events[0].Action)
	require.Equal(t, ActionTerminate, events[1].Action)
}

func TestReadFilters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w := NewWriter(path)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	w.Log(Event{Time: base, Action: ActionSpawn, Target: "agent-1"})
	w.Log(Event{Time: base.Add(time.Hour), Action: ActionTerminate, Target: "agent-1"})
	w.Log(Event{Time: base.Add(2 * time.Hour), Action: ActionSpawn, Target: "agent-2"})
	w.Log(Event{Time: base.Add(3 * time.Hour), Action: ActionPipelineStart, Target: "pipe-x"})

	t.Run("by action", func(t *testing.T) {
		got, err := Read(path, Filter{Action: ActionSpawn})
		require.NoError(t, err)
		require.Len(t, got, 2)
		require.Equal(t, "agent-1", got[0].Target)
		require.Equal(t, "agent-2", got[1].Target)
	})

	t.Run("by target substring", func(t *testing.T) {
		got, err := Read(path, Filter{Target: "agent"})
		require.NoError(t, err)
		require.Len(t, got, 3)
	})

	t.Run("by time window", func(t *testing.T) {
		got, err := Read(path, Filter{Since: base.Add(time.Hour), Until: base.Add(2 * time.Hour)})
		require.NoError(t, err)
		require.Len(t, got, 2)
		require.Equal(t, ActionTerminate, got[0].Action)
		require.Equal(t, ActionSpawn, got[1].Action)
	})

	t.Run("limit keeps most recent", func(t *testing.T) {
		got, err := Read(path, Filter{Limit: 2})
		require.NoError(t, err)
		require.Len(t, got, 2)
		require.Equal(t, "agent-2", got[0].Target)
		require.Equal(t, "pipe-x", got[1].Target)
	})

	t.Run("limit applies after filter", func(t *testing.T) {
		got, err := Read(path, Filter{Action: ActionSpawn, Limit: 1})
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, "agent-2", got[0].Target)
	})
}
