package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func agySess(id, workdir, pinned string, st store.Status) *agentstore.Agent {
	return &agentstore.Agent{ID: id, TmuxSession: id, AiCli: "antigravity", Workdir: workdir, AICLISessionID: pinned, Status: st}
}

func TestTranscriptAmbiguity(t *testing.T) {
	lc, _, _ := newSwapLC(t)
	a := agySess("a", "/w", "", store.StatusWorking)
	b := agySess("b", "/w", "", store.StatusWorking)
	all := []*agentstore.Agent{a, b}
	lc.PeerSessions = func() []*agentstore.Agent { return all }

	require.True(t, lc.transcriptAmbiguous(a), "unpinned + live same-workdir peer")
	require.Equal(t, "", lc.transcriptPath(a), "no transcript attributed")

	// Once pinned the session resolves by id, never ambiguous.
	a.AICLISessionID = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	require.False(t, lc.transcriptAmbiguous(a))

	// Single session in the directory: unchanged (not ambiguous).
	b.AICLISessionID = ""
	all = []*agentstore.Agent{b}
	require.False(t, lc.transcriptAmbiguous(b))

	// Peers that are dead, in another dir, or another backend don't count.
	all = []*agentstore.Agent{b, agySess("c", "/w", "", store.StatusDone), agySess("d", "/other", "", store.StatusWorking)}
	other := agySess("e", "/w", "", store.StatusWorking)
	other.AiCli = "claude"
	all = append(all, other)
	require.False(t, lc.transcriptAmbiguous(b))

	// Legacy backends without log discovery are never gated; no PeerSessions = legacy.
	cl := agySess("f", "/w", "", store.StatusWorking)
	cl.AiCli = "claude"
	all = []*agentstore.Agent{cl, agySess("g", "/w", "", store.StatusWorking)}
	require.False(t, lc.transcriptAmbiguous(cl))
	lc.PeerSessions = nil
	require.False(t, lc.transcriptAmbiguous(a))
}

func TestHotSwapRefusesAmbiguousTranscript(t *testing.T) {
	lc, _, sess := newSwapLC(t)
	sess.AiCli = "antigravity"
	sess.AICLISessionID = ""
	sess.Status = store.StatusWorking
	peer := agySess("other", sess.Workdir, "", store.StatusWorking)
	lc.PeerSessions = func() []*agentstore.Agent { return []*agentstore.Agent{sess, peer} }

	_, err := lc.HotSwap(context.Background(), sess, SwapRequest{Backend: "claude", Reason: SwapReasonQuota})
	require.ErrorIs(t, err, ErrAmbiguousTranscript)
	_, statErr := os.Stat(filepath.Join(sess.Workdir, ".warden", "handoff-"+sess.ID+".md"))
	require.True(t, os.IsNotExist(statErr), "no handoff written from the wrong conversation")
}

func TestSessionLogFile(t *testing.T) {
	lc, _, _ := newSwapLC(t)
	agy := lc.backendFor("antigravity")
	require.Equal(t, "", lc.sessionLogFile(agy, "a"), "disabled without SessionLogsDir")

	lc.SessionLogsDir = filepath.Join(t.TempDir(), "logs")
	p := lc.sessionLogFile(agy, "a")
	require.Equal(t, filepath.Join(lc.SessionLogsDir, "a.log"), p)
	require.Equal(t, p, lc.SessionLogPath("a"))

	// A stale log from a previous launch of the same agent id is cleared.
	require.NoError(t, os.WriteFile(p, []byte("old"), 0o644))
	lc.sessionLogFile(agy, "a")
	_, statErr := os.Stat(p)
	require.True(t, os.IsNotExist(statErr))

	// Backends without log discovery get none.
	require.Equal(t, "", lc.sessionLogFile(lc.backendFor("claude"), "a"))
}
