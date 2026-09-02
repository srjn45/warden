package autopilot

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLedgerStateValidAndParse(t *testing.T) {
	for i, st := range CanonicalLedgerStates {
		require.True(t, st.Valid(), st)
		require.Equal(t, i, st.SegmentIndex(), st)
		got, err := ParseLedgerState(" " + strings.ToUpper(string(st)) + " ")
		require.NoError(t, err, st)
		require.Equal(t, st, got)
	}

	require.False(t, LedgerState("").Valid())
	require.False(t, LedgerState("fixing").Valid())
	require.False(t, LedgerState("replanned").Valid())
	require.False(t, LedgerState("active").Valid())
	require.Equal(t, -1, LedgerState("fixing").SegmentIndex())

	_, err := ParseLedgerState("fixing")
	require.ErrorIs(t, err, ErrInvalidLedgerState)
	_, err = ParseLedgerState("")
	require.ErrorIs(t, err, ErrInvalidLedgerState)
}

func TestCanonicalLedgerStatesAreTUIOrder(t *testing.T) {
	require.Equal(t, []LedgerState{
		LedgerPending, LedgerAssigned, LedgerInProgress,
		LedgerPROpen, LedgerGated, LedgerLanded,
	}, CanonicalLedgerStates)
}
