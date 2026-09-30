// Package plansync defines the PlanSyncProvider boundary for future Warden Hub
// synchronization of canonical ScrivaDB Plan revisions.
//
// Design freeze: docs/specs/2026-09-30-scrivadb-canonical-plans.md D6.
// Protocol: docs/specs/2026-09-30-plan-hub-sync-boundary.md.
//
// This package is intentionally separate from internal/planexport (repository
// replica export / sync_to_repo). Hub sync transports canonical Plan revisions
// across machines and teams; repo export publishes inert YAML via Git/PR.
// Neither replaces origin/main as authority for shipped code.
//
// Default installation uses Local() — a no-op provider that never dials the
// network. Authorization, account management, remote transport, background
// replication, and Hub UI are out of scope for this phase.
package plansync

import (
	"context"
	"fmt"
	"time"

	"github.com/srjn45/warden/internal/planstore"
)

// SchemaVersion is the v1 Hub sync envelope schema_version.
const SchemaVersion = 1

// Provider names returned by PlanSyncProvider.Name.
const (
	ProviderLocal = "local"
	ProviderFake  = "fake"
	// ProviderHub is reserved for a future remote implementation; not shipped.
	ProviderHub = "hub"
)

// Visibility is the intended audience for a synced Plan revision. A future Hub
// authorizes against this claim; local/no-op stores it without enforcing it.
type Visibility string

const (
	VisibilityPrivate      Visibility = "private"
	VisibilityProject      Visibility = "project"
	VisibilityTeam         Visibility = "team"
	VisibilityOrganization Visibility = "organization"
)

// Valid reports whether v is a known visibility.
func (v Visibility) Valid() bool {
	switch v {
	case VisibilityPrivate, VisibilityProject, VisibilityTeam, VisibilityOrganization:
		return true
	}
	return false
}

// ChangeOriginKind classifies who produced the revision offered for sync.
type ChangeOriginKind string

const (
	OriginLocalDaemon ChangeOriginKind = "local_daemon"
	OriginLocalUser   ChangeOriginKind = "local_user"
	OriginLocalAgent  ChangeOriginKind = "local_agent"
	// OriginHub is reserved for revisions that arrived from a future Hub pull.
	OriginHub ChangeOriginKind = "hub"
)

// Scope identifies organization/team/project tenancy for discovery and sync.
// Empty OrganizationID/TeamID means "unset / local-only" — valid for Local().
type Scope struct {
	OrganizationID string `json:"organization_id,omitempty"`
	TeamID         string `json:"team_id,omitempty"`
	ProjectID      string `json:"project_id,omitempty"`
}

// LinkedArtifact is a code artifact associated with the Plan (branch or PR).
// Used for team discovery overlap; not an execution authority signal.
type LinkedArtifact struct {
	Kind string `json:"kind"` // "branch" | "pull_request"
	Ref  string `json:"ref"`  // branch name, PR URL, or "#n"
}

// ChangeOrigin records where a revision change came from.
type ChangeOrigin struct {
	Kind    ChangeOriginKind `json:"kind"`
	ActorID string           `json:"actor_id,omitempty"` // user, agent, or daemon id
	NodeID  string           `json:"node_id,omitempty"`  // optional local daemon node
}

// Envelope is the versioned Hub sync payload around one canonical Plan revision.
// It carries the identity and discovery metadata a future Hub needs without
// embedding credentials, disposable worktree paths, or full task prompts.
type Envelope struct {
	SchemaVersion int `json:"schema_version"`

	Scope     Scope  `json:"scope"`
	ProjectID string `json:"project_id"`
	PlanID    string `json:"plan_id"`
	Name      string `json:"name,omitempty"`

	Revision    int64  `json:"revision"`
	ContentHash string `json:"content_hash"`

	Visibility Visibility `json:"visibility"`
	OwnerID    string     `json:"owner_id,omitempty"`

	Lifecycle planstore.PlanStatus `json:"lifecycle"`

	Artifacts []LinkedArtifact `json:"artifacts,omitempty"`
	Origin    ChangeOrigin     `json:"origin"`

	// ConflictToken is the opaque optimistic-concurrency token for Hub merge.
	// Local builders set it to ConflictToken(revision, content_hash).
	ConflictToken string `json:"conflict_token"`

	// SyncedAt / RemoteID mirror planstore.Plan hub seams; filled only by a
	// future Hub provider after a successful remote round-trip.
	SyncedAt *time.Time `json:"synced_at,omitempty"`
	RemoteID string     `json:"remote_id,omitempty"`
}

// PullQuery filters Pull results. Empty PlanID / Statuses means "all".
type PullQuery struct {
	Scope    Scope
	PlanID   string
	Statuses []planstore.PlanStatus
}

// PlanSyncProvider transports canonical Plan revision envelopes. Repo export
// (planexport.Syncer) is a different concern and must not implement this
// interface.
type PlanSyncProvider interface {
	// Name identifies the implementation ("local", "fake", "hub", …).
	Name() string
	// Enabled reports whether remote sync is active. Local/no-op returns false.
	Enabled() bool
	// Push offers a local canonical revision for synchronization.
	Push(ctx context.Context, env Envelope) error
	// Pull fetches envelopes matching q from the provider.
	Pull(ctx context.Context, q PullQuery) ([]Envelope, error)
	// Discover lists plans in scope for team discovery (typically pending and
	// in_progress) so users see work already underway before duplicating it.
	Discover(ctx context.Context, scope Scope, statuses []planstore.PlanStatus) ([]Envelope, error)
}

// ConflictToken builds the v1 opaque conflict token from revision + hash.
func ConflictToken(revision int64, contentHash string) string {
	return fmt.Sprintf("%d:%s", revision, contentHash)
}

// Default returns the provider used by a default (offline) installation.
// It is always Local() — never a network-capable Hub client.
func Default() PlanSyncProvider {
	return Local()
}
