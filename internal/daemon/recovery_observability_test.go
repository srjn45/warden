package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/capacity"
)

func TestRecoveryEvidenceEventDetailOrderingAndRedaction(t *testing.T) {
	ev := RecoveryEvidence{
		Source:             capacity.SourceUsage,
		Provider:           "claude",
		AccountFingerprint: "sha256:abcdef0123456789ffff",
		Route:              "sonnet",
		BucketKey:          "weekly",
		Freshness:          capacity.ImpactFresh,
		Reason:             "mandatory_bucket_exhausted Bearer sk-SECRETTOKEN99",
	}
	line := ev.eventDetail(map[string]string{
		"generation": "3",
		"candidate":  "codex/gpt-5",
		"outcome":    "launch_ok",
	})
	require.True(t, strings.HasPrefix(line, "generation=3"), "generation must lead for stable ordering: %s", line)
	require.Contains(t, line, "trigger_source=usage")
	require.Contains(t, line, "capacity_domain=claude/sha256:abcdef012")
	require.Contains(t, line, "bucket_key=weekly")
	require.Contains(t, line, "freshness=fresh")
	require.Contains(t, line, "candidate=codex/gpt-5")
	require.NotContains(t, line, "sk-")
	require.NotContains(t, line, "SECRETTOKEN")
	require.Contains(t, line, "[REDACTED]")

	// Ordering: generation before trigger_source before candidate.
	gi := strings.Index(line, "generation=")
	ti := strings.Index(line, "trigger_source=")
	ci := strings.Index(line, "candidate=")
	require.True(t, gi < ti && ti < ci, "expected generation < trigger_source < candidate in %q", line)
}

func TestOnHardLimitEmitsAuditAndStampsObservability(t *testing.T) {
	c, st, _ := recoveryFixture(t, map[string][]backendusage.Limit{
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(95)}},
		"codex":  {{ID: "primary", Scope: "primary", Label: "Primary", UsedPercent: used(10)}},
	})
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	c.SetAudit(audit.NewWriter(auditPath))

	c.ArmEvidence("agent-1", RecoveryEvidence{
		Source:             capacity.SourceUsage,
		Provider:           "claude",
		AccountFingerprint: "sha256:deadbeefcafef00d",
		Route:              "sonnet",
		BucketKey:          "weekly",
		Freshness:          capacity.ImpactFresh,
		Reason:             "mandatory_bucket_exhausted",
	})
	require.True(t, c.OnHardLimit(st.snapSession("agent-1"), time.Now().Add(time.Hour)))

	sess := st.snapSession("agent-1")
	require.NotNil(t, sess.BackendRecovery)
	require.Equal(t, capacity.SourceUsage, sess.BackendRecovery.TriggerSource)
	require.Equal(t, "claude/sha256:deadbeefc/sonnet", sess.BackendRecovery.CapacityDomain)
	require.Equal(t, "weekly", sess.BackendRecovery.BucketKey)
	require.Equal(t, capacity.ImpactFresh, sess.BackendRecovery.Freshness)

	var sawStarted bool
	for _, ev := range sess.Events {
		require.NotContains(t, ev.Detail, "sk-")
		if ev.Type == audit.ActionRecoveryStarted {
			sawStarted = true
			require.Contains(t, ev.Detail, "trigger_source=usage")
			require.Contains(t, ev.Detail, "generation=")
		}
	}
	require.True(t, sawStarted, "recovery_started durable agent event must be present")

	events, err := audit.Read(auditPath, audit.Filter{Action: audit.ActionRecoveryStarted})
	require.NoError(t, err)
	require.NotEmpty(t, events, "append-only audit must record recovery_started (not only journal)")
	require.Equal(t, "agent-1", events[0].Target)
	require.Equal(t, capacity.SourceUsage, events[0].Detail["trigger_source"])
	require.NotContains(t, events[0].Detail, "token")

	// Cancel async advance so it cannot write to a cleaned-up temp audit path.
	c.Supersede(context.Background(), "agent-1", "manual_stop")
}

func TestWaitingForCapacityWritesAuditReason(t *testing.T) {
	reset := time.Now().Add(time.Hour).UTC()
	c, st, _ := recoveryFixture(t, map[string][]backendusage.Limit{
		"codex":       {{ID: "short", Scope: "short", Label: "Short", UsedPercent: used(100), ResetsAt: &reset}},
		"claude":      {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(100), ResetsAt: &reset}},
		"antigravity": {{ID: "other", Scope: "other", Label: "Other", UsedPercent: used(100), ResetsAt: &reset}},
	})
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	c.SetAudit(audit.NewWriter(auditPath))
	c.ArmEvidence("agent-1", RecoveryEvidence{Source: capacity.SourceBanner, Reason: "pane_hard_limit"})
	require.True(t, c.OnHardLimit(st.snapSession("agent-1"), reset))

	require.Eventually(t, func() bool {
		events, _ := audit.Read(auditPath, audit.Filter{Action: audit.ActionWaitingForCapacity})
		return len(events) > 0
	}, 2*time.Second, 10*time.Millisecond)

	events, err := audit.Read(auditPath, audit.Filter{Action: audit.ActionWaitingForCapacity})
	require.NoError(t, err)
	require.NotEmpty(t, events)
	require.Equal(t, "no_eligible_candidate", events[0].Detail["reason"])
	require.NotEmpty(t, events[0].Detail["next_retry"])
}

func TestQuotaImpactAuditVocabulary(t *testing.T) {
	s := &Server{}
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	s.SetAudit(audit.NewWriter(auditPath))
	s.recordRecoveryAudit(audit.ActionQuotaImpactCalculated, "usage", summarizeImpact(capacity.ImpactResult{
		ExhaustedBuckets: []capacity.ExhaustedBucket{{Provider: "claude", BucketKey: "weekly"}},
		AffectedAgents:   []capacity.AffectedAgent{{AgentID: "a1"}},
		SkippedAgents:    []capacity.SkippedAgent{{AgentID: "a2", Reason: capacity.SkipUnboundLegacy}},
	}))
	s.recordRecoveryAudit(audit.ActionQuotaBucketExhausted, "claude/fp", map[string]string{
		"bucket_key": "weekly",
		"token":      "sk-nope",
	})

	impact, err := audit.Read(auditPath, audit.Filter{Action: audit.ActionQuotaImpactCalculated})
	require.NoError(t, err)
	require.Len(t, impact, 1)
	require.Equal(t, "1", impact[0].Detail["exhausted_buckets"])
	require.Equal(t, "1", impact[0].Detail["affected_agents"])

	exhausted, err := audit.Read(auditPath, audit.Filter{Action: audit.ActionQuotaBucketExhausted})
	require.NoError(t, err)
	require.Len(t, exhausted, 1)
	require.Equal(t, "weekly", exhausted[0].Detail["bucket_key"])
	require.NotContains(t, exhausted[0].Detail, "token")
}

func TestSupersedeEmitsRecoverySupersededAudit(t *testing.T) {
	c, st, _ := recoveryFixture(t, map[string][]backendusage.Limit{
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
		"codex":  {{ID: "primary", Scope: "primary", Label: "Primary", UsedPercent: used(10)}},
	})
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	c.SetAudit(audit.NewWriter(auditPath))
	require.True(t, c.OnHardLimit(st.snapSession("agent-1"), time.Now().Add(time.Hour)))
	c.Supersede(context.Background(), "agent-1", "manual_stop")

	events, err := audit.Read(auditPath, audit.Filter{Action: audit.ActionRecoverySuperseded})
	require.NoError(t, err)
	require.NotEmpty(t, events)
	require.Equal(t, "manual_stop", events[0].Detail["action"])

	sess := st.snapSession("agent-1")
	var saw bool
	for _, ev := range sess.Events {
		if ev.Type == audit.ActionRecoverySuperseded {
			saw = true
			require.Contains(t, ev.Detail, "action=manual_stop")
		}
	}
	require.True(t, saw)
}
