package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSessionIsTerminal(t *testing.T) {
	// The zero value (records that predate the Kind field) reads as an agent, so
	// no migration is needed and existing sessions keep AI-centric behavior.
	require.False(t, (&Session{}).IsTerminal(), "empty Kind must read as agent")
	require.False(t, (&Session{Kind: KindAgent}).IsTerminal())
	require.True(t, (&Session{Kind: KindTerminal}).IsTerminal())
}

func TestSessionKindJSONOmitEmpty(t *testing.T) {
	// An agent session (empty Kind) must omit the field entirely so on-disk
	// records are byte-identical to pre-Kind ones; a terminal serializes "kind".
	b, err := json.Marshal(Session{ID: "a"})
	require.NoError(t, err)
	require.NotContains(t, string(b), `"kind"`, "agent session must omit kind")

	b, err = json.Marshal(Session{ID: "t", Kind: KindTerminal})
	require.NoError(t, err)
	require.Contains(t, string(b), `"kind":"terminal"`)

	// Absent kind decodes back to the agent zero value.
	var s Session
	require.NoError(t, json.Unmarshal([]byte(`{"id":"a"}`), &s))
	require.False(t, s.IsTerminal())
}

func TestSessionJSONRoundTrip(t *testing.T) {
	s := Session{
		ID:              "PROJ-350",
		Type:            TypeDevelopment,
		Ticket:          "PROJ-350",
		TmuxSession:     "PROJ-350",
		ClaudeSessionID: "11111111-1111-4111-8111-111111111111",
		Repo:            "/repo",
		Worktree:        ".worktrees/PROJ-350",
		Branch:          "PROJ-350",
		Prompt:          "do a security review of the auth module",
		Workdir:         "/Users/me/warden-agents/agent-a1b2",
		Subject:         "review auth module for security",
		Status:          StatusSpawning,
		PID:             123,
		Events:          []Event{{Type: "SessionStart"}},
	}
	raw, err := json.Marshal(s)
	require.NoError(t, err)

	var got Session
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, "PROJ-350", got.ID)
	require.Equal(t, TypeDevelopment, got.Type)
	require.Equal(t, StatusSpawning, got.Status)
	require.Len(t, got.Events, 1)
	require.Equal(t, "SessionStart", got.Events[0].Type)
	require.Equal(t, "do a security review of the auth module", got.Prompt)
	require.Equal(t, "/Users/me/warden-agents/agent-a1b2", got.Workdir)
	require.Equal(t, "review auth module for security", got.Subject)
	require.Equal(t, "11111111-1111-4111-8111-111111111111", got.ClaudeSessionID)
}

func TestNormalizeTags(t *testing.T) {
	// Trim, lowercase, drop blanks, dedup, preserve first-seen order.
	got := NormalizeTags([]string{"  Backend ", "urgent", "backend", "", "  ", "URGENT"})
	require.Equal(t, []string{"backend", "urgent"}, got)

	// All-empty input collapses to nil so untagged sessions stay JSON-omitted.
	require.Nil(t, NormalizeTags([]string{"", "   "}))
	require.Nil(t, NormalizeTags(nil))
}

func TestSessionHasTag(t *testing.T) {
	s := Session{Tags: NormalizeTags([]string{"Backend", "Urgent"})}
	require.True(t, s.HasTag("backend"))
	require.True(t, s.HasTag(" URGENT "), "match is case- and whitespace-insensitive")
	require.False(t, s.HasTag("frontend"))
	require.False(t, s.HasTag(""), "blank tag never matches")

	var untagged Session
	require.False(t, untagged.HasTag("anything"), "nil Tags is safe")
}

func TestSessionTagsJSONOmitemptyBackwardCompat(t *testing.T) {
	// A session with no tags must not emit a "tags" key, so records that
	// predate the field round-trip identically.
	raw, err := json.Marshal(Session{ID: "x"})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "\"tags\"")

	// Legacy JSON without tags unmarshals to a nil slice.
	var got Session
	require.NoError(t, json.Unmarshal([]byte(`{"id":"x"}`), &got))
	require.Nil(t, got.Tags)

	// A tagged session round-trips its labels.
	tagged := Session{ID: "y", Tags: []string{"backend", "urgent"}}
	raw, err = json.Marshal(tagged)
	require.NoError(t, err)
	var back Session
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, []string{"backend", "urgent"}, back.Tags)
}

func TestStatusValid(t *testing.T) {
	require.True(t, StatusWorking.Valid())
	require.False(t, Status("bogus").Valid())
}

func TestTypeNormalizeAndWorktreePolicy(t *testing.T) {
	// Known types keep their value; unknown collapses to "other".
	require.Equal(t, TypeDevelopment, NormalizeType("development"))
	require.Equal(t, Type(""), NormalizeType("totally-made-up"))

	// Default worktree policy (Phase 0a: every write-agent isolates by default).
	require.True(t, TypeDevelopment.DefaultWorktree())
	require.True(t, TypePRReview.DefaultWorktree())
	require.True(t, TypeDebugCI.DefaultWorktree())
	require.True(t, TypeCode.DefaultWorktree())
	require.True(t, TypeTests.DefaultWorktree())
	require.True(t, TypeDocs.DefaultWorktree())
	require.True(t, TypeWebsite.DefaultWorktree())
	require.False(t, TypeOther.DefaultWorktree()) // free-form catch-all never isolates
	require.False(t, TypeSpike.DefaultWorktree()) // opt-in via --worktree, not default
	require.False(t, TypeAnalysis.DefaultWorktree())
}

func TestSessionExitCodeJSONRoundTrip(t *testing.T) {
	code := 137
	s := Session{ID: "a", Status: StatusErrored, ExitCode: &code}
	b, err := json.Marshal(s)
	require.NoError(t, err)
	require.Contains(t, string(b), `"exit_code":137`)

	// nil ExitCode is omitted entirely (omitempty).
	b2, err := json.Marshal(Session{ID: "b", Status: StatusWorking})
	require.NoError(t, err)
	require.NotContains(t, string(b2), "exit_code")

	var back Session
	require.NoError(t, json.Unmarshal(b, &back))
	require.NotNil(t, back.ExitCode)
	require.Equal(t, 137, *back.ExitCode)
}

func TestSessionAutoRestartFieldsJSON(t *testing.T) {
	at := time.Date(2026, 6, 10, 1, 2, 3, 0, time.UTC)
	s := Session{ID: "a", AutoRestart: true, RestartCount: 2, LastRestartAt: &at}
	b, err := json.Marshal(s)
	require.NoError(t, err)
	require.Contains(t, string(b), `"auto_restart":true`)
	require.Contains(t, string(b), `"restart_count":2`)

	var back Session
	require.NoError(t, json.Unmarshal(b, &back))
	require.True(t, back.AutoRestart)
	require.Equal(t, 2, back.RestartCount)
	require.Equal(t, at, back.LastRestartAt.UTC())

	b2, err := json.Marshal(Session{ID: "b"})
	require.NoError(t, err)
	require.NotContains(t, string(b2), "restart_count")
	require.NotContains(t, string(b2), "last_restart_at")
	require.NotContains(t, string(b2), "auto_restart")
}

func TestStatusRateLimited_Valid(t *testing.T) {
	if !StatusRateLimited.Valid() {
		t.Error("StatusRateLimited should be valid")
	}
}

func TestStatusRateLimited_Serialization(t *testing.T) {
	s := Session{
		ID:     "test",
		Status: StatusRateLimited,
	}

	b, err := json.Marshal(s)
	require.NoError(t, err)
	require.Contains(t, string(b), `"status":"rate_limited"`)

	var back Session
	require.NoError(t, json.Unmarshal(b, &back))
	require.Equal(t, StatusRateLimited, back.Status)
}

func TestSession_RateLimitFields(t *testing.T) {
	now := time.Now()
	restoreAt := now.Add(1 * time.Hour)

	s := Session{
		ID:                  "test",
		RateLimitedAt:       &now,
		RateLimitRestoreAt:  &restoreAt,
		RateLimitRetryCount: 2,
	}

	b, err := json.Marshal(s)
	require.NoError(t, err)
	require.Contains(t, string(b), `"rate_limited_at"`)
	require.Contains(t, string(b), `"rate_limit_restore_at"`)
	require.Contains(t, string(b), `"rate_limit_retry_count":2`)

	var back Session
	require.NoError(t, json.Unmarshal(b, &back))
	require.NotNil(t, back.RateLimitedAt)
	require.NotNil(t, back.RateLimitRestoreAt)
	require.Equal(t, 2, back.RateLimitRetryCount)
}

func TestSession_RateLimitFields_Omitempty(t *testing.T) {
	s := Session{ID: "test"}

	b, err := json.Marshal(s)
	require.NoError(t, err)
	require.NotContains(t, string(b), "rate_limited_at")
	require.NotContains(t, string(b), "rate_limit_restore_at")
	require.NotContains(t, string(b), "rate_limit_retry_count")
}
