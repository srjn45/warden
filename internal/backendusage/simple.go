package backendusage

import (
	"context"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
)

type GenericAdapter struct {
	ID  string
	Now func() time.Time
}

func (a GenericAdapter) BackendID() string { return a.ID }
func (a GenericAdapter) Fetch(_ context.Context, b backendstore.Backend) Result {
	now := clock(a.Now)
	if !b.Installed {
		return notInstalled(b.ID, now)
	}
	return unsupported(b.ID, "backend has no structured usage adapter", now)
}

func binary(b backendstore.Backend, fallback string) string {
	if b.BinaryPath != "" {
		return b.BinaryPath
	}
	return fallback
}
func clock(fn func() time.Time) time.Time {
	if fn != nil {
		return fn().UTC()
	}
	return time.Now().UTC()
}
func notInstalled(id string, now time.Time) Result {
	return Result{BackendID: id, Status: StatusNotInstalled, Usage: []Limit{}, ObservedAt: now, Error: &ProviderError{Code: "not_installed", Message: "backend CLI is not installed"}}
}
func unauthenticated(id string, now time.Time) Result {
	return Result{BackendID: id, Status: StatusUnauthenticated, Usage: []Limit{}, ObservedAt: now, Error: &ProviderError{Code: "unauthenticated", Message: "backend CLI is not authenticated"}}
}
func malformed(id string, now time.Time) Result {
	return Result{BackendID: id, Status: StatusError, Usage: []Limit{}, ObservedAt: now, Error: &ProviderError{Code: "invalid_response", Message: "backend returned an invalid structured response"}}
}
func commandFailure(id string, ctx context.Context, now time.Time) Result {
	st, code, msg := StatusUnavailable, "unavailable", "backend usage probe is unavailable"
	if ctx.Err() != nil {
		st, code, msg = StatusTimeout, "timeout", "backend usage probe timed out"
	}
	return Result{BackendID: id, Status: st, Usage: []Limit{}, ObservedAt: now, Error: &ProviderError{Code: code, Message: msg}}
}
