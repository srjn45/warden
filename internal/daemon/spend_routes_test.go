package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/spend"
	"github.com/stretchr/testify/require"
)

// newSpendServer builds a minimal Server with an enabled spend tracker backed by
// a temp dir, optionally pre-loaded with one session's billed tokens so the
// budget gate / report have something to price.
func newSpendServer(t *testing.T, model, repo string, in, out int) *Server {
	t.Helper()
	st, err := spend.NewStore(t.TempDir())
	require.NoError(t, err)
	if in > 0 || out > 0 {
		require.NoError(t, st.Record("seed", model, repo, in, out))
	}
	return &Server{savingsOn: true, spend: st}
}

func TestGetSpendReport(t *testing.T) {
	// 1M Opus input = $5.
	s := newSpendServer(t, "opus", "/x", 1_000_000, 0)
	resp, err := s.GetSpend(context.Background(), oapi.GetSpendRequestObject{})
	require.NoError(t, err)
	report, ok := resp.(oapi.GetSpend200JSONResponse)
	require.True(t, ok)
	require.InDelta(t, 5.0, report.TotalUSD, 1e-9)
	require.Len(t, report.ByAgent, 1)
	require.Equal(t, "seed", report.ByAgent[0].Key)
}

func TestGetSpendDisabled403(t *testing.T) {
	s := &Server{savingsOn: false}
	_, err := s.GetSpend(context.Background(), oapi.GetSpendRequestObject{})
	require.Error(t, err)
	var ae apiError
	require.True(t, errors.As(err, &ae))
	require.Equal(t, http.StatusForbidden, ae.code)
}

// TestBudgetGateWarns drives the full spawn path: with spend already over the
// daily cap and the budget gate on, a non-forced spawn returns 428.
func TestBudgetGateWarns(t *testing.T) {
	fs := newFakeStore()
	st, err := spend.NewStore(t.TempDir())
	require.NoError(t, err)
	// 2M Opus input = $10, over a $1 daily cap.
	require.NoError(t, st.Record("seed", "opus", "/x", 2_000_000, 0))
	s := &Server{store: fs, life: &fakeLife{}, savingsOn: true, spend: st,
		budgetGate: true, budgetDailyUSD: 1}

	resp := postSpawn(t, s, SpawnRequest{Prompt: "do x", Cwd: t.TempDir()})
	defer resp.Body.Close()
	require.Equal(t, http.StatusPreconditionRequired, resp.StatusCode)
	var out struct {
		ConfirmationRequired bool `json:"confirmation_required"`
		Verdict              struct {
			Elevated bool   `json:"elevated"`
			Reason   string `json:"reason"`
		} `json:"verdict"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.True(t, out.ConfirmationRequired)
	require.True(t, out.Verdict.Elevated)
	require.Contains(t, out.Verdict.Reason, "budget")
}

func TestBudgetGateForceBypasses(t *testing.T) {
	fs := newFakeStore()
	st, _ := spend.NewStore(t.TempDir())
	_ = st.Record("seed", "opus", "/x", 2_000_000, 0)
	s := &Server{store: fs, life: &fakeLife{}, savingsOn: true, spend: st,
		budgetGate: true, budgetDailyUSD: 1}

	resp := postSpawn(t, s, SpawnRequest{Prompt: "do x", Cwd: t.TempDir(), Force: true})
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestBudgetGateUnderCapProceeds(t *testing.T) {
	fs := newFakeStore()
	st, _ := spend.NewStore(t.TempDir())
	_ = st.Record("seed", "opus", "/x", 1_000, 0) // pennies, under any cap
	s := &Server{store: fs, life: &fakeLife{}, savingsOn: true, spend: st,
		budgetGate: true, budgetDailyUSD: 25}

	resp := postSpawn(t, s, SpawnRequest{Prompt: "do x", Cwd: t.TempDir()})
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestBudgetGateOffProceeds(t *testing.T) {
	fs := newFakeStore()
	st, _ := spend.NewStore(t.TempDir())
	_ = st.Record("seed", "opus", "/x", 9_000_000, 0)
	s := &Server{store: fs, life: &fakeLife{}, savingsOn: true, spend: st,
		budgetGate: false, budgetDailyUSD: 1}

	resp := postSpawn(t, s, SpawnRequest{Prompt: "do x", Cwd: t.TempDir()})
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
}
