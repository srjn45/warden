package plansync

import (
	"context"
	"sync"

	"github.com/srjn45/warden/internal/planstore"
)

// FakeProvider is an in-memory PlanSyncProvider for contract tests. It never
// dials the network. Push is create-or-idempotent: a matching ConflictToken
// succeeds; a different token against an existing PlanID returns ConflictError.
// Use Replace to simulate a resolved newer revision in tests.
type FakeProvider struct {
	mu   sync.Mutex
	byID map[string]Envelope
}

// NewFake returns an empty FakeProvider.
func NewFake() *FakeProvider {
	return &FakeProvider{byID: map[string]Envelope{}}
}

// Name implements PlanSyncProvider.
func (*FakeProvider) Name() string { return ProviderFake }

// Enabled implements PlanSyncProvider. Fake is "enabled" for contract tests so
// callers can exercise Push/Pull/Discover without a real Hub.
func (*FakeProvider) Enabled() bool { return true }

// Push implements PlanSyncProvider.
func (f *FakeProvider) Push(_ context.Context, env Envelope) error {
	if err := validateEnvelope(env); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if prev, ok := f.byID[env.PlanID]; ok {
		if prev.ConflictToken == env.ConflictToken {
			return nil // idempotent
		}
		return &ConflictError{
			PlanID:   env.PlanID,
			Expected: prev.ConflictToken,
			Actual:   env.ConflictToken,
		}
	}
	cp := env
	if cp.Scope.ProjectID == "" {
		cp.Scope.ProjectID = cp.ProjectID
	}
	f.byID[env.PlanID] = cp
	return nil
}

// Replace stores env unconditionally (test helper for advancing revisions after
// a conflict is "resolved"). Still validates the envelope.
func (f *FakeProvider) Replace(_ context.Context, env Envelope) error {
	if err := validateEnvelope(env); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := env
	if cp.Scope.ProjectID == "" {
		cp.Scope.ProjectID = cp.ProjectID
	}
	f.byID[env.PlanID] = cp
	return nil
}

// Pull implements PlanSyncProvider.
func (f *FakeProvider) Pull(_ context.Context, q PullQuery) ([]Envelope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Envelope, 0, len(f.byID))
	for _, env := range f.byID {
		if !matchScope(env, q.Scope) {
			continue
		}
		if q.PlanID != "" && env.PlanID != q.PlanID {
			continue
		}
		if !matchStatuses(env.Lifecycle, q.Statuses) {
			continue
		}
		out = append(out, env)
	}
	return out, nil
}

// Discover implements PlanSyncProvider.
func (f *FakeProvider) Discover(ctx context.Context, scope Scope, statuses []planstore.PlanStatus) ([]Envelope, error) {
	if len(statuses) == 0 {
		statuses = []planstore.PlanStatus{planstore.PlanStatusPending, planstore.PlanStatusInProgress}
	}
	return f.Pull(ctx, PullQuery{Scope: scope, Statuses: statuses})
}

// Len returns the number of stored envelopes (test helper).
func (f *FakeProvider) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.byID)
}

func matchScope(env Envelope, scope Scope) bool {
	if scope.OrganizationID != "" && env.Scope.OrganizationID != scope.OrganizationID {
		return false
	}
	if scope.TeamID != "" && env.Scope.TeamID != scope.TeamID {
		return false
	}
	if scope.ProjectID != "" {
		pid := env.Scope.ProjectID
		if pid == "" {
			pid = env.ProjectID
		}
		if pid != scope.ProjectID {
			return false
		}
	}
	return true
}

func matchStatuses(got planstore.PlanStatus, want []planstore.PlanStatus) bool {
	if len(want) == 0 {
		return true
	}
	for _, s := range want {
		if got == s {
			return true
		}
	}
	return false
}
