package backendusage

import (
	"context"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
)

type Status string

const (
	StatusOK              Status = "ok"
	StatusUnsupported     Status = "unsupported"
	StatusUnauthenticated Status = "unauthenticated"
	StatusRateLimited     Status = "rate_limited"
	StatusUnavailable     Status = "unavailable"
	StatusTimeout         Status = "timeout"
	StatusError           Status = "error"
	StatusNotInstalled    Status = "not_installed"
)

type Account struct {
	Plan        string `json:"plan,omitempty"`
	LoginMethod string `json:"login_method,omitempty"`
	// ProfileFingerprint is an opaque, non-secret stable account/profile key.
	// Adapters must never put an email address, token, or raw provider account ID here.
	ProfileFingerprint string `json:"profile_fingerprint,omitempty"`
	Label              string `json:"-"`
}

// CapacityDomain identifies the non-secret provider capacity that a result
// describes. It is deliberately additive while quota bindings are introduced:
// an empty profile means the adapter could only identify the provider default.
type CapacityDomain struct {
	Provider           string `json:"provider"`
	ProfileFingerprint string `json:"profile_fingerprint,omitempty"`
	Route              string `json:"route,omitempty"`
}

func (d CapacityDomain) Key() string {
	return d.Provider + "\x00" + d.ProfileFingerprint + "\x00" + d.Route
}

type BucketState string

const (
	BucketAvailable BucketState = "available"
	BucketExhausted BucketState = "exhausted"
	BucketUnknown   BucketState = "unknown"
)

// CapacityBucket is the durable, normalized form of a provider Limit.
type CapacityBucket struct {
	Key              string      `json:"key"`
	Scope            string      `json:"scope,omitempty"`
	State            BucketState `json:"state"`
	UsedPercent      *float64    `json:"used_percent,omitempty"`
	RemainingPercent *float64    `json:"remaining_percent,omitempty"`
	DurationMinutes  *int        `json:"duration_minutes,omitempty"`
	ResetsAt         *time.Time  `json:"resets_at,omitempty"`
}

type Freshness string

const (
	FreshnessFresh   Freshness = "fresh"
	FreshnessStale   Freshness = "stale"
	FreshnessUnknown Freshness = "unknown"
)

// UsageSnapshot is one durable observation. Native details are intentionally
// limited to the normalized error code; response bodies and account labels are
// never stored.
type UsageSnapshot struct {
	Revision      uint64           `json:"revision"`
	Domain        CapacityDomain   `json:"domain"`
	ObservedAt    time.Time        `json:"observed_at"`
	RecordedAt    time.Time        `json:"recorded_at"`
	SourceStatus  Status           `json:"source_status"`
	Authoritative bool             `json:"authoritative"`
	Freshness     Freshness        `json:"freshness"`
	Buckets       []CapacityBucket `json:"buckets"`
	ErrorCode     string           `json:"error_code,omitempty"`
}

// Limit is one distinct provider-owned allowance/reset window. ID is unique
// within its backend; Scope identifies what the limit applies to. Unknown
// measurements and selectors are explicit JSON nulls rather than invented data.
type Limit struct {
	ID               string     `json:"id"`
	Scope            string     `json:"scope"`
	Label            string     `json:"label"`
	ModelFamilies    []string   `json:"model_families"`
	Models           []string   `json:"models"`
	UsedPercent      *float64   `json:"used_percent"`
	RemainingPercent *float64   `json:"remaining_percent,omitempty"`
	DurationMinutes  *int       `json:"duration_minutes,omitempty"`
	ResetsAt         *time.Time `json:"resets_at"`
	LimitState       *string    `json:"limit_state,omitempty"`
}

type ProviderError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Result struct {
	BackendID  string         `json:"-"`
	Status     Status         `json:"status"`
	Account    *Account       `json:"account,omitempty"`
	Usage      []Limit        `json:"usage"`
	Error      *ProviderError `json:"error,omitempty"`
	ObservedAt time.Time      `json:"observed_at"`
}

type BackendResult struct {
	ID         string         `json:"id"`
	Tier       string         `json:"tier"`
	Installed  bool           `json:"installed"`
	Enabled    bool           `json:"enabled"`
	Status     Status         `json:"status"`
	Account    *Account       `json:"account,omitempty"`
	Usage      []Limit        `json:"usage"`
	ObservedAt time.Time      `json:"observed_at"`
	Cached     bool           `json:"cached"`
	Stale      bool           `json:"stale"`
	Error      *ProviderError `json:"error,omitempty"`
}

type Snapshot struct {
	SchemaVersion int             `json:"schema_version"`
	GeneratedAt   time.Time       `json:"generated_at"`
	Backends      []BackendResult `json:"backends"`
}

type Adapter interface {
	BackendID() string
	Fetch(context.Context, backendstore.Backend) Result
}

func unsupported(id, message string, now time.Time) Result {
	return Result{BackendID: id, Status: StatusUnsupported, Usage: []Limit{}, ObservedAt: now,
		Error: &ProviderError{Code: "usage_unsupported", Message: message}}
}
