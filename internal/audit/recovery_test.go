package audit

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSafeDetailDropsForbiddenKeysAndRedactsSecrets(t *testing.T) {
	got := SafeDetail(map[string]string{
		"provider":        "claude",
		"token":           "sk-abc123456789",
		"api_token":       "should-drop",
		"authorization":   "Bearer xyz",
		"bucket_key":      "weekly",
		"reason":          "ok Bearer sk-live-ABCDEFGH and user@example.com",
		"capacity_domain": FormatDomain("claude", "sha256:abcdef0123456789ffff", "sonnet"),
	})
	require.Equal(t, "claude", got["provider"])
	require.Equal(t, "weekly", got["bucket_key"])
	require.NotContains(t, got, "token")
	require.NotContains(t, got, "api_token")
	require.NotContains(t, got, "authorization")
	require.NotContains(t, got["reason"], "sk-")
	require.NotContains(t, got["reason"], "Bearer")
	require.NotContains(t, got["reason"], "@example.com")
	require.Contains(t, got["reason"], "[REDACTED]")
	require.Equal(t, "claude/sha256:abcdef012/sonnet", got["capacity_domain"])
}

func TestSafeDetailNil(t *testing.T) {
	require.Nil(t, SafeDetail(nil))
}

func TestRecoveryAuditActionsAreStableVocabulary(t *testing.T) {
	want := []string{
		ActionUsageSnapshotReceived,
		ActionUsageSnapshotFailed,
		ActionQuotaBucketExhausted,
		ActionQuotaImpactCalculated,
		ActionRecoveryStarted,
		ActionCandidateAttempted,
		ActionCandidateResult,
		ActionWaitingForCapacity,
		ActionRecoveryStabilized,
		ActionRecoverySuperseded,
	}
	for _, a := range want {
		require.NotEmpty(t, a)
		require.NotContains(t, a, " ")
	}
}

func TestWriterAcceptsRecoveryActions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w := NewWriter(path)
	w.Log(Event{
		Action: ActionRecoveryStarted,
		Target: "agent-1",
		Detail: SafeDetail(map[string]string{
			"trigger_source":  "usage",
			"capacity_domain": "claude/sha256:abcd",
			"bucket_key":      "weekly",
			"token":           "sk-should-never-land",
		}),
	})
	events, err := Read(path, Filter{Action: ActionRecoveryStarted})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, "agent-1", events[0].Target)
	require.Equal(t, "usage", events[0].Detail["trigger_source"])
	require.NotContains(t, events[0].Detail, "token")
}
