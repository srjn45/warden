package backendusage

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
)

const (
	claudeUsageEndpoint = "https://api.anthropic.com/api/oauth/usage"
	claudeOAuthBeta     = "oauth-2025-04-20"
	// claudeSessionWindowMinutes is the fixed length of Claude's rolling
	// session ("five_hour") allowance.
	claudeSessionWindowMinutes = 300
)

// ClaudeAdapter reports Claude's single session (5-hour) usage window, read from
// the OAuth-authenticated usage endpoint the `claude` CLI's /usage pager uses.
// It authenticates with the local Claude Code OAuth access token and, like the
// other subscription adapters, degrades gracefully to a single null-percentage
// row when the endpoint is unreachable rather than failing the whole probe.
type ClaudeAdapter struct {
	Now       func() time.Time
	ReadFile  func(string) ([]byte, error)
	CredsPath func() string
	Doer      HTTPDoer
	Endpoint  string
}

func (a ClaudeAdapter) BackendID() string { return "claude" }

func (a ClaudeAdapter) Fetch(ctx context.Context, b backendstore.Backend) Result {
	now := clock(a.Now)
	if !b.Installed {
		return notInstalled(b.ID, now)
	}

	creds, err := a.readCredentials()
	if err != nil || creds == nil || creds.AccessToken == "" {
		return unauthenticated(b.ID, now)
	}

	account := &Account{Plan: claudePlanLabel(creds.SubscriptionType), LoginMethod: "claude.ai"}
	res := Result{
		BackendID:  b.ID,
		Status:     StatusOK,
		Account:    account,
		Usage:      claudeLimits(nil, nil),
		ObservedAt: now,
	}

	body, status, err := a.fetchUsage(ctx, creds.AccessToken)
	if err != nil || status >= 400 || len(body) == 0 {
		return res
	}

	used, reset, ok := parseClaudeUsage(body)
	if !ok {
		return res
	}

	res.Usage = claudeLimits(used, reset)
	if used != nil && *used >= 100 {
		res.Status = StatusRateLimited
		res.Error = &ProviderError{Code: "rate_limited", Message: "provider reports that the session usage limit has been reached"}
	}
	return res
}

type claudeCredentials struct {
	AccessToken      string
	SubscriptionType string
}

func (a ClaudeAdapter) readCredentials() (*claudeCredentials, error) {
	rf := a.ReadFile
	if rf == nil {
		rf = os.ReadFile
	}
	pathFn := a.CredsPath
	if pathFn == nil {
		pathFn = defaultClaudeCredentialsPath
	}
	p := pathFn()
	if p == "" {
		return nil, os.ErrNotExist
	}
	raw, err := rf(p)
	if err != nil {
		return nil, err
	}
	var cf struct {
		ClaudeAiOauth struct {
			AccessToken      string `json:"accessToken"`
			SubscriptionType string `json:"subscriptionType"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &cf); err != nil {
		return nil, err
	}
	if cf.ClaudeAiOauth.AccessToken == "" {
		return nil, os.ErrNotExist
	}
	return &claudeCredentials{
		AccessToken:      cf.ClaudeAiOauth.AccessToken,
		SubscriptionType: cf.ClaudeAiOauth.SubscriptionType,
	}, nil
}

func defaultClaudeCredentialsPath() string {
	home := userHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", ".credentials.json")
}

func (a ClaudeAdapter) doer() HTTPDoer {
	if a.Doer != nil {
		return a.Doer
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (a ClaudeAdapter) fetchUsage(ctx context.Context, accessToken string) ([]byte, int, error) {
	endpoint := a.Endpoint
	if endpoint == "" {
		endpoint = claudeUsageEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", claudeOAuthBeta)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.doer().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return body, resp.StatusCode, err
}

// parseClaudeUsage extracts the session ("five_hour") window's utilization and
// reset time. Utilization is already a 0-100 percentage in the response.
func parseClaudeUsage(body []byte) (used *float64, reset *time.Time, ok bool) {
	var resp struct {
		FiveHour *struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    *string  `json:"resets_at"`
		} `json:"five_hour"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, nil, false
	}
	if resp.FiveHour == nil {
		return nil, nil, false
	}

	if resp.FiveHour.Utilization != nil {
		u := *resp.FiveHour.Utilization
		if u < 0 {
			u = 0
		}
		if u > 100 {
			u = 100
		}
		u = math.Round(u*100) / 100
		used = &u
	}

	if resp.FiveHour.ResetsAt != nil && *resp.FiveHour.ResetsAt != "" {
		if t, err := time.Parse(time.RFC3339, *resp.FiveHour.ResetsAt); err == nil {
			ut := t.UTC()
			reset = &ut
		}
	}

	return used, reset, true
}

func claudeLimits(used *float64, reset *time.Time) []Limit {
	dur := claudeSessionWindowMinutes
	var remaining *float64
	if used != nil && *used >= 0 && *used <= 100 {
		v := math.Round((100-*used)*100) / 100
		remaining = &v
	}
	var state *string
	if used != nil && *used >= 100 {
		v := "reached"
		state = &v
	}
	return []Limit{{
		ID:               "claude:session",
		Scope:            "session",
		Label:            "Session (5-hour)",
		UsedPercent:      used,
		RemainingPercent: remaining,
		DurationMinutes:  &dur,
		ResetsAt:         reset,
		LimitState:       state,
	}}
}

func claudePlanLabel(sub string) string {
	s := strings.TrimSpace(sub)
	switch strings.ToLower(s) {
	case "":
		return ""
	case "pro":
		return "Pro"
	case "max":
		return "Max"
	case "team":
		return "Team"
	case "enterprise":
		return "Enterprise"
	case "free":
		return "Free"
	default:
		return s
	}
}
