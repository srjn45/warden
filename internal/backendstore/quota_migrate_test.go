package backendstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStore_MigrateLegacyBareQuotaKey(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	// Plant a true legacy record: key = backendID, no scope field.
	legacy := BackendQuota{
		BackendID:  "legacy-cli",
		WindowType: WindowDaily,
		QuotaLimit: 99,
		UsedAmount: 11,
		LastReset:  now,
		UpdatedAt:  now,
	}
	rec, err := toRecord(legacy)
	require.NoError(t, err)
	_, _, err = s.quotasCol.InsertWithKey("legacy-cli", rec)
	require.NoError(t, err)

	q, err := s.GetQuota("legacy-cli")
	require.NoError(t, err)
	require.Equal(t, DefaultQuotaScope, q.Scope)
	require.Equal(t, 99.0, q.QuotaLimit)
	require.Equal(t, 11.0, q.UsedAmount)

	// Legacy bare key is gone; scoped key exists.
	_, err = s.quotasCol.GetByKey("legacy-cli")
	require.Error(t, err)
	_, err = s.quotasCol.GetByKey(quotaKey("legacy-cli", DefaultQuotaScope))
	require.NoError(t, err)
}
