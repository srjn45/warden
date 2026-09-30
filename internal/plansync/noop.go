package plansync

import (
	"context"

	"github.com/srjn45/warden/internal/planstore"
)

// LocalProvider is the default offline PlanSyncProvider. Every method is a
// pure in-process no-op: no dialers, HTTP clients, goroutines, or background
// replication. Push succeeds without persisting; Pull and Discover return empty
// slices. This is what a fresh warden install uses until a Hub provider is
// explicitly configured (not part of this phase).
type LocalProvider struct{}

// Local returns the default offline provider.
func Local() *LocalProvider {
	return &LocalProvider{}
}

// Name implements PlanSyncProvider.
func (*LocalProvider) Name() string { return ProviderLocal }

// Enabled implements PlanSyncProvider. Always false — remote sync is off.
func (*LocalProvider) Enabled() bool { return false }

// Push implements PlanSyncProvider. Accepts well-formed envelopes and discards them.
func (*LocalProvider) Push(_ context.Context, env Envelope) error {
	if err := validateEnvelope(env); err != nil {
		return err
	}
	return nil
}

// Pull implements PlanSyncProvider. Always returns an empty list.
func (*LocalProvider) Pull(_ context.Context, _ PullQuery) ([]Envelope, error) {
	return nil, nil
}

// Discover implements PlanSyncProvider. Always returns an empty list.
func (*LocalProvider) Discover(_ context.Context, _ Scope, _ []planstore.PlanStatus) ([]Envelope, error) {
	return nil, nil
}

func validateEnvelope(env Envelope) error {
	if env.SchemaVersion != SchemaVersion {
		return errSchema(env.SchemaVersion)
	}
	if env.PlanID == "" {
		return errMissing("plan_id")
	}
	if env.ProjectID == "" && env.Scope.ProjectID == "" {
		return errMissing("project_id")
	}
	if env.Revision < 1 {
		return errMissing("revision")
	}
	if env.ContentHash == "" {
		return errMissing("content_hash")
	}
	if env.ConflictToken == "" {
		return errMissing("conflict_token")
	}
	if env.Visibility != "" && !env.Visibility.Valid() {
		return errInvalidVisibility(env.Visibility)
	}
	return nil
}
