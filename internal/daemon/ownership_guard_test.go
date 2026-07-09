package daemon

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/auth"
	"github.com/srjn45/warden/internal/poller"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// ctxWithActor returns a context carrying an HTTP request whose actor header names
// the calling agent (empty ⇒ no header, i.e. a human/web caller), matching what
// stashRequest installs in production.
func ctxWithActor(actor string) context.Context {
	r := &http.Request{Header: http.Header{}}
	if actor != "" {
		r.Header.Set(auth.ActorHeader, actor)
	}
	return context.WithValue(context.Background(), requestCtxKey{}, r)
}

// requireForbidden asserts err is a 403 not_owned refusal.
func requireForbidden(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var ae apiError
	require.True(t, errors.As(err, &ae), "expected an apiError")
	require.Equal(t, http.StatusForbidden, ae.code)
	require.Contains(t, ae.msg, "not_owned")
}

func TestGuardOwnership(t *testing.T) {
	fs := newFakeStore()
	// The run's brain (role autopilot, owning run:ap-1).
	brain := &store.Session{ID: "brain-1", Role: autopilotBrainRole, Tags: []string{"autopilot", "run:ap-1"}}
	// A worker of this run.
	owned := &store.Session{ID: "worker-1", Tags: []string{"autopilot", "run:ap-1"}}
	// A worker of a different run.
	foreign := &store.Session{ID: "worker-2", Tags: []string{"autopilot", "run:ap-2"}}
	// A hand-launched agent with no autopilot tags.
	manual := &store.Session{ID: "manual-1"}
	// An ordinary (non-brain) agent making a call.
	human := &store.Session{ID: "dev-1"}
	for _, s := range []*store.Session{brain, owned, foreign, manual, human} {
		require.NoError(t, fs.Insert(context.Background(), s))
	}
	srv := &Server{store: fs}

	t.Run("brain may act on its own run's worker", func(t *testing.T) {
		require.NoError(t, srv.guardOwnership(ctxWithActor("brain-1"), owned))
	})
	t.Run("brain may act on itself", func(t *testing.T) {
		require.NoError(t, srv.guardOwnership(ctxWithActor("brain-1"), brain))
	})
	t.Run("brain is refused a foreign run's worker", func(t *testing.T) {
		requireForbidden(t, srv.guardOwnership(ctxWithActor("brain-1"), foreign))
	})
	t.Run("brain is refused a manual agent", func(t *testing.T) {
		requireForbidden(t, srv.guardOwnership(ctxWithActor("brain-1"), manual))
	})
	t.Run("non-brain agent caller is unaffected", func(t *testing.T) {
		require.NoError(t, srv.guardOwnership(ctxWithActor("dev-1"), manual))
	})
	t.Run("human caller (no actor header) is unaffected", func(t *testing.T) {
		require.NoError(t, srv.guardOwnership(ctxWithActor(""), manual))
	})
	t.Run("unknown actor id is unaffected", func(t *testing.T) {
		require.NoError(t, srv.guardOwnership(ctxWithActor("ghost"), manual))
	})
}

// TestGuardOwnershipBrainWithoutRunTagOwnsNothing proves a brain that somehow
// carries no run tag is denied every foreign target (the safe default).
func TestGuardOwnershipBrainWithoutRunTag(t *testing.T) {
	fs := newFakeStore()
	brain := &store.Session{ID: "brain-x", Role: autopilotBrainRole, Tags: []string{"autopilot"}}
	target := &store.Session{ID: "worker-x", Tags: []string{"autopilot", "run:ap-1"}}
	require.NoError(t, fs.Insert(context.Background(), brain))
	require.NoError(t, fs.Insert(context.Background(), target))
	srv := &Server{store: fs}
	requireForbidden(t, srv.guardOwnership(ctxWithActor("brain-x"), target))
}

// TestInstallDefaultAutoApprovePolicy proves the §10 seam installs a generous
// default only when the owner has configured no rules, and never clobbers an
// existing policy.
func TestInstallDefaultAutoApprovePolicy(t *testing.T) {
	t.Run("installs when owner has no rules", func(t *testing.T) {
		p := poller.New(nil, 0)
		rt := autopilotRuntime{s: &Server{poller: p}}
		rt.InstallDefaultAutoApprovePolicy()
		got := p.AutoApprovePolicySnapshot()
		require.True(t, got.Enabled, "the default policy enables auto-approve")
		require.True(t, got.HasRules(), "the default policy carries an allow rule")
	})

	t.Run("no-op when owner already configured rules", func(t *testing.T) {
		p := poller.New(nil, 0)
		owner := approval.Policy{Rules: approval.Rules{Deny: []approval.Rule{{Tool: "Bash"}}}}
		p.SetAutoApprovePolicy(owner)
		rt := autopilotRuntime{s: &Server{poller: p}}
		rt.InstallDefaultAutoApprovePolicy()
		got := p.AutoApprovePolicySnapshot()
		require.False(t, got.Enabled, "an owner-configured policy is left untouched")
		require.Len(t, got.Rules.Deny, 1)
		require.Empty(t, got.Rules.Allow)
	})
}
