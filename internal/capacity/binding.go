// Package capacity owns the daemon's non-secret view of provider capacity.
// It deliberately contains identifiers suitable for persistence and matching,
// never credentials, account names, or provider response payloads.
package capacity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/srjn45/warden/internal/backendstore"
)

const LegacyUnbound = "unbound_legacy"

// CapacityDomain identifies one provider account/profile and route. AccountFingerprint
// is an opaque, one-way value; it must never contain an account identifier or token.
type CapacityDomain struct {
	Provider           string   `json:"provider"`
	AiCli              string   `json:"ai_cli"`
	AccountFingerprint string   `json:"account_fingerprint"`
	Route              string   `json:"route"`
	BucketKeys         []string `json:"bucket_keys"`
}

// QuotaBinding is the daemon-owned association between an agent and the capacity
// buckets all of its selected route requires. Bucket ordering is significant.
type QuotaBinding struct {
	Domain           CapacityDomain `json:"domain"`
	MandatoryBuckets []string       `json:"mandatory_buckets"`
}

// Catalog is the small backend-registry seam used to resolve a selected route.
type Catalog interface {
	GetModel(backendID, modelID string) (backendstore.ModelEntry, error)
}

// ProfileSource returns the currently selected non-secret account/profile label
// for an AI CLI. It is hashed before persistence. Nil means the provider's default
// local profile; it is intentionally deterministic, not a guess from credentials.
type ProfileSource func(context.Context, string) (string, error)

// Resolver centralizes provider-specific route-to-bucket mapping. Lifecycle calls
// it only after the daemon has selected the AI CLI and model.
type Resolver struct {
	catalog Catalog
	profile ProfileSource
}

func NewResolver(catalog Catalog, profile ProfileSource) *Resolver {
	return &Resolver{catalog: catalog, profile: profile}
}

// Resolve creates a safe persisted binding. It never exposes the supplied profile
// text: the stable fingerprint is a truncated SHA-256 digest.
func (r *Resolver) Resolve(ctx context.Context, aiCli, model string) (*QuotaBinding, error) {
	aiCli = strings.TrimSpace(aiCli)
	if aiCli == "" {
		aiCli = "claude"
	}
	profile := "default"
	if r.profile != nil {
		if p, err := r.profile(ctx, aiCli); err != nil {
			return nil, err
		} else if strings.TrimSpace(p) != "" {
			profile = strings.TrimSpace(p)
		}
	}
	scope := backendstore.DefaultQuotaScope
	if r.catalog != nil && model != "" {
		if entry, err := r.catalog.GetModel(aiCli, model); err == nil && entry.QuotaScope != "" {
			scope = entry.QuotaScope
		}
	}
	buckets := providerBuckets(aiCli, scope)
	return &QuotaBinding{Domain: CapacityDomain{Provider: aiCli, AiCli: aiCli, AccountFingerprint: fingerprint(aiCli, profile), Route: model, BucketKeys: append([]string(nil), buckets...)}, MandatoryBuckets: append([]string(nil), buckets...)}, nil
}

func fingerprint(provider, profile string) string {
	s := sha256.Sum256([]byte("warden-capacity-v1:" + provider + ":" + profile))
	return "sha256:" + hex.EncodeToString(s[:12])
}

// providerBuckets is the sole provider-specific mapping point. Current usage
// adapters expose scoped buckets; new multi-bucket routes extend this switch.
func providerBuckets(aiCli, scope string) []string {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		scope = backendstore.DefaultQuotaScope
	}
	switch aiCli {
	case "claude", "codex", "antigravity", "cursor":
		return []string{scope}
	default:
		return []string{scope}
	}
}
