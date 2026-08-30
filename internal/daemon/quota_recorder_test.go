package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// fakeQuotaLife satisfies the daemon Lifecycle interface for the quota recorder,
// resolving each session to a transcript file by id.
type fakeQuotaLife struct {
	Lifecycle
	paths map[string]string
}

func (f *fakeQuotaLife) TranscriptPath(sess *store.Session) string { return f.paths[sess.ID] }

// writeTranscript writes a Claude-style transcript JSONL with the given
// per-turn (input, output) usage pairs and returns its path.
func writeTranscript(t *testing.T, dir, name string, turns [][2]int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	var body string
	for _, tn := range turns {
		body += `{"type":"assistant","message":{"usage":{"input_tokens":` +
			strconv.Itoa(tn[0]) + `,"output_tokens":` + strconv.Itoa(tn[1]) + `}}}` + "\n"
	}
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestQuotaRecorder_RecordsDeltaAfterBaseline(t *testing.T) {
	dir := t.TempDir()
	bs, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	defer bs.Close()

	// A live claude agent with a transcript totalling 150 billed tokens.
	tpath := writeTranscript(t, dir, "claude.jsonl", [][2]int{{100, 50}})
	sess := &store.Session{ID: "a1", Status: store.StatusWorking, Backend: "claude", Model: "opus"}

	st := &rateLimitStore{sessions: map[string]*store.Session{"a1": sess}}
	life := &fakeQuotaLife{paths: map[string]string{"a1": tpath}}
	srv := &Server{store: st, life: life, backends: bs}

	last := make(map[string]int)

	// First sample seeds the baseline; nothing is recorded (no back-fill). The
	// store seeds default quota profiles, so claude's window exists but stays at 0.
	srv.recordQuotaOnce(context.Background(), last)
	require.Equal(t, 150, last["a1"], "baseline is the current cumulative total")
	q0, gerr := bs.GetQuota("claude")
	require.NoError(t, gerr)
	require.Equal(t, float64(0), q0.UsedAmount, "the seeding sample records no usage")

	// The agent bills another 300 tokens; the delta is recorded into the window.
	writeTranscript(t, dir, "claude.jsonl", [][2]int{{100, 50}, {200, 100}}) // total 450
	srv.recordQuotaOnce(context.Background(), last)

	require.Equal(t, 450, last["a1"], "baseline advances to the new total")
	q, gerr := bs.GetQuota("claude")
	require.NoError(t, gerr)
	require.Equal(t, float64(300), q.UsedAmount, "only the positive delta since the baseline is recorded")
}

func TestQuotaRecorder_SkipsTerminalAndUnknownBackend(t *testing.T) {
	dir := t.TempDir()
	bs, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	defer bs.Close()

	tpath := writeTranscript(t, dir, "t.jsonl", [][2]int{{100, 100}})
	// A terminal session and a backend-less session: neither accrues quota.
	term := &store.Session{ID: "term", Status: store.StatusWorking, Kind: store.KindTerminal, Backend: "claude"}
	noBackend := &store.Session{ID: "nb", Status: store.StatusWorking}

	st := &rateLimitStore{sessions: map[string]*store.Session{"term": term, "nb": noBackend}}
	life := &fakeQuotaLife{paths: map[string]string{"term": tpath, "nb": tpath}}
	srv := &Server{store: st, life: life, backends: bs}

	last := make(map[string]int)
	srv.recordQuotaOnce(context.Background(), last)
	srv.recordQuotaOnce(context.Background(), last) // a second tick would record a delta if either were tracked

	require.Empty(t, last, "neither a terminal nor a backend-less session is tracked")
	// The store's default quota profiles exist but must stay at zero usage: no
	// usage is attributed to a terminal or a backend-less session.
	quotas, lerr := bs.ListQuotas()
	require.NoError(t, lerr)
	for _, q := range quotas {
		require.Equal(t, float64(0), q.UsedAmount, "skipped sessions accrue no usage (%s)", q.BackendID)
	}
}

func TestQuotaRecorder_PrunesDeadSessions(t *testing.T) {
	dir := t.TempDir()
	bs, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	defer bs.Close()

	tpath := writeTranscript(t, dir, "a.jsonl", [][2]int{{10, 10}})
	sess := &store.Session{ID: "a1", Status: store.StatusWorking, Backend: "claude"}
	st := &rateLimitStore{sessions: map[string]*store.Session{"a1": sess}}
	life := &fakeQuotaLife{paths: map[string]string{"a1": tpath}}
	srv := &Server{store: st, life: life, backends: bs}

	last := make(map[string]int)
	srv.recordQuotaOnce(context.Background(), last)
	require.Contains(t, last, "a1")

	// Session disappears; the next tick prunes its baseline so the map stays bounded.
	delete(st.sessions, "a1")
	srv.recordQuotaOnce(context.Background(), last)
	require.NotContains(t, last, "a1", "a gone session's baseline is pruned")
}
