package autopilot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// managerReconciliationCase is intentionally a data-only fixture seam. The
// verified-manager-adoption task consumes these observations through fake
// adapters; this baseline must never start tmux or a daemon.
type managerReconciliationCase struct {
	Name                     string `json:"name"`
	SlotScope                string `json:"slot_scope"`
	RunID                    string `json:"run_id"`
	PlanID                   string `json:"plan_id"`
	ProjectID                string `json:"project_id"`
	TmuxSession              string `json:"tmux_session"`
	TmuxAlive                bool   `json:"tmux_alive"`
	Record                   string `json:"record"`
	Metadata                 string `json:"metadata"`
	ExpectedDisposition      string `json:"expected_disposition"`
	ExpectedRunTransition    string `json:"expected_run_transition"`
	ExpectedLedgerTransition string `json:"expected_ledger_transition"`
}

func loadManagerReconciliationCases(t *testing.T) []managerReconciliationCase {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "manager_reconciliation_cases.json"))
	require.NoError(t, err)
	var cases []managerReconciliationCase
	require.NoError(t, json.Unmarshal(b, &cases))
	return cases
}

func TestManagerReconciliationFixturesCoverContractClasses(t *testing.T) {
	cases := loadManagerReconciliationCases(t)
	require.Len(t, cases, 5)
	byName := make(map[string]managerReconciliationCase, len(cases))
	for _, tc := range cases {
		require.NotEmpty(t, tc.Name)
		require.NotEmpty(t, tc.SlotScope)
		require.Equal(t, ManagerSlotID(tc.SlotScope), tc.TmuxSession)
		require.NotEmpty(t, tc.RunID)
		require.NotEmpty(t, tc.PlanID)
		require.NotEmpty(t, tc.ProjectID)
		require.NotEmpty(t, tc.ExpectedDisposition)
		require.NotEmpty(t, tc.ExpectedRunTransition)
		require.NotEmpty(t, tc.ExpectedLedgerTransition)
		_, duplicate := byName[tc.Name]
		require.Falsef(t, duplicate, "duplicate fixture %q", tc.Name)
		byName[tc.Name] = tc
	}

	require.Equal(t, "retain_registered", byName["live_registered"].ExpectedDisposition)
	require.Equal(t, "adopt_verified", byName["live_unregistered"].ExpectedDisposition)
	require.Equal(t, "mark_missing", byName["dead_slot"].ExpectedDisposition)
	require.Equal(t, "refuse_foreign", byName["foreign_session"].ExpectedDisposition)
	require.Equal(t, "refuse_ambiguous", byName["ambiguous_session"].ExpectedDisposition)
	require.False(t, byName["dead_slot"].TmuxAlive)
	require.True(t, byName["live_unregistered"].TmuxAlive)
}
