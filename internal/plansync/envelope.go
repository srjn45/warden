package plansync

import (
	"fmt"
	"sort"
	"strings"

	"github.com/srjn45/warden/internal/planstore"
)

// EnvelopeOptions supplies Hub-facing metadata that is not on the Plan record
// today (visibility, owner, tenancy, change origin). Local builders pass these
// explicitly; a future Hub may derive some of them from auth context.
type EnvelopeOptions struct {
	Scope      Scope
	Visibility Visibility
	OwnerID    string
	Origin     ChangeOrigin
}

// EnvelopeFromPlan builds a v1 sync envelope from a canonical Plan revision.
// It never reads repository YAML. ContentHash is refreshed when empty.
func EnvelopeFromPlan(p *planstore.Plan, opts EnvelopeOptions) (Envelope, error) {
	if p == nil {
		return Envelope{}, fmt.Errorf("plansync: plan is nil")
	}
	if strings.TrimSpace(p.ID) == "" {
		return Envelope{}, fmt.Errorf("plansync: plan id is required")
	}
	vis := opts.Visibility
	if vis == "" {
		vis = VisibilityProject
	}
	if !vis.Valid() {
		return Envelope{}, fmt.Errorf("plansync: invalid visibility %q", vis)
	}
	origin := opts.Origin
	if origin.Kind == "" {
		origin.Kind = OriginLocalDaemon
	}

	hash := p.ContentHash
	if hash == "" {
		hash = planstore.ComputeContentHash(p)
	}

	scope := opts.Scope
	if scope.ProjectID == "" {
		scope.ProjectID = p.ProjectID
	}

	env := Envelope{
		SchemaVersion: SchemaVersion,
		Scope:         scope,
		ProjectID:     p.ProjectID,
		PlanID:        p.ID,
		Name:          p.Name,
		Revision:      p.Revision,
		ContentHash:   hash,
		Visibility:    vis,
		OwnerID:       opts.OwnerID,
		Lifecycle:     p.Status,
		Artifacts:     artifactsFromPlan(p),
		Origin:        origin,
		ConflictToken: ConflictToken(p.Revision, hash),
		// SyncedAt / RemoteID intentionally left empty — only a Hub provider fills them.
		SyncedAt: p.SyncedAt,
		RemoteID: p.RemoteID,
	}
	return env, nil
}

func artifactsFromPlan(p *planstore.Plan) []LinkedArtifact {
	branches, prs := planstore.LinkedArtifacts(p)
	out := make([]LinkedArtifact, 0, len(branches)+len(prs))
	branchKeys := make([]string, 0, len(branches))
	for b := range branches {
		branchKeys = append(branchKeys, b)
	}
	sort.Strings(branchKeys)
	for _, b := range branchKeys {
		out = append(out, LinkedArtifact{Kind: "branch", Ref: b})
	}
	prKeys := make([]string, 0, len(prs))
	for pr := range prs {
		prKeys = append(prKeys, pr)
	}
	sort.Strings(prKeys)
	for _, pr := range prKeys {
		out = append(out, LinkedArtifact{Kind: "pull_request", Ref: pr})
	}
	return out
}
