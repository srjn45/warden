package backendusage

import (
	"context"
	"encoding/json"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
)

type ClaudeAdapter struct {
	Runner CommandRunner
	Now    func() time.Time
}

func (a ClaudeAdapter) BackendID() string { return "claude" }
func (a ClaudeAdapter) Fetch(ctx context.Context, b backendstore.Backend) Result {
	now := clock(a.Now)
	if !b.Installed {
		return notInstalled(b.ID, now)
	}
	r := a.Runner
	if r == nil {
		r = execRunner{}
	}
	out, err := r.Output(ctx, binary(b, "claude"), "auth", "status", "--json")
	var v struct {
		LoggedIn         bool   `json:"loggedIn"`
		AuthMethod       string `json:"authMethod"`
		APIProvider      string `json:"apiProvider"`
		SubscriptionType string `json:"subscriptionType"`
	}
	decoded := json.Unmarshal(out, &v) == nil
	if err != nil {
		if decoded && !v.LoggedIn {
			return unauthenticated(b.ID, now)
		}
		return commandFailure(b.ID, ctx, now)
	}
	if !decoded {
		return malformed(b.ID, now)
	}
	if !v.LoggedIn {
		return unauthenticated(b.ID, now)
	}
	login := v.AuthMethod
	if login == "" {
		login = v.APIProvider
	}
	res := unsupported(b.ID, "installed Claude CLI has no structured usage interface", now)
	res.Account = &Account{Plan: v.SubscriptionType, LoginMethod: login}
	return res
}

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
