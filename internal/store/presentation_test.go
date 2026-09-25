package store

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPresentedStatus(t *testing.T) {
	code := 137
	zero := 0
	for _, tt := range []struct {
		status Status
		exit   *int
		want   string
	}{
		{StatusSpawning, nil, "pending"}, {StatusWorking, nil, "busy"},
		{StatusWaitingForInput, nil, "need-input"}, {StatusIdle, nil, "idle"},
		{StatusDone, nil, "done"}, {StatusOrphaned, nil, "orphaned"},
		{StatusRateLimited, nil, "rate_limited"}, {StatusErrored, nil, "orphaned"},
		{StatusErrored, &code, "done"}, {StatusErrored, &zero, "done"},
		{Status("busy"), nil, "busy"}, {Status("need-input"), nil, "need-input"},
		{Status(""), nil, "pending"},
	} {
		require.Equal(t, tt.want, PresentedStatus(tt.status, tt.exit))
		data, err := json.Marshal(Session{Status: tt.status, ExitCode: tt.exit})
		require.NoError(t, err)
		var back Session
		require.NoError(t, json.Unmarshal(data, &back))
		require.Equal(t, tt.status, back.Status, "presentation must not mutate the wire status")
	}
}
