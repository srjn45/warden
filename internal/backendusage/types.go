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
	Label       string `json:"-"`
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
