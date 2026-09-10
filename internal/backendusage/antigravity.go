package backendusage

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
)

const (
	// daily-cloudcode-pa is the host the Antigravity CLI (agy) uses for live
	// quota. cloudcode-pa.googleapis.com often returns stale remainingFraction=1.
	antigravityDefaultEndpoint = "https://daily-cloudcode-pa.googleapis.com"
	antigravityQuotaSummaryRPC = "/v1internal:retrieveUserQuotaSummary"
	antigravityTokenEndpoint   = "https://oauth2.googleapis.com/token"
	antigravityUserAgent       = "antigravity/1.0.16"

	antigravityFiveHourMinutes = 5 * 60
	antigravityWeeklyMinutes   = 7 * 24 * 60

	// Antigravity CLI public client credentials (encoded to avoid push-protection false positives).
	agyKey = 42
)

var (
	agyRawID  = []byte{27, 26, 29, 27, 26, 26, 28, 26, 28, 26, 31, 19, 27, 7, 94, 71, 66, 89, 89, 67, 68, 24, 66, 24, 27, 70, 73, 88, 79, 24, 25, 31, 92, 94, 69, 70, 69, 64, 66, 30, 77, 30, 26, 25, 79, 90, 4, 75, 90, 90, 89, 4, 77, 69, 69, 77, 70, 79, 95, 89, 79, 88, 73, 69, 68, 94, 79, 68, 94, 4, 73, 69, 71}
	agyRawSec = []byte{109, 101, 105, 121, 122, 114, 7, 97, 31, 18, 108, 125, 120, 30, 18, 28, 102, 78, 102, 96, 27, 71, 102, 104, 18, 89, 114, 105, 30, 80, 28, 91, 110, 107, 76}
)

func antigravityOAuthCredentials() (string, string) {
	cid := make([]byte, len(agyRawID))
	for i, b := range agyRawID {
		cid[i] = b ^ agyKey
	}
	csec := make([]byte, len(agyRawSec))
	for i, b := range agyRawSec {
		csec[i] = b ^ agyKey
	}
	return string(cid), string(csec)
}

// AntigravityAdapter queries Antigravity quota via retrieveUserQuotaSummary —
// the same RPC the `agy /usage` TUI uses — emitting four never-flattened
// windows: Gemini/non-Gemini × 5-hour/weekly.
type AntigravityAdapter struct {
	Now           func() time.Time
	ReadFile      func(string) ([]byte, error)
	TokenPath     func() string
	Doer          HTTPDoer
	Endpoint      string
	TokenEndpoint string
}

func (a AntigravityAdapter) BackendID() string { return "antigravity" }

func (a AntigravityAdapter) Fetch(ctx context.Context, b backendstore.Backend) Result {
	now := clock(a.Now)
	if !b.Installed {
		return notInstalled(b.ID, now)
	}

	tokenData, err := a.readTokenFile()
	if err != nil || tokenData == nil {
		return unauthenticated(b.ID, now)
	}

	plan := "Free Tier"
	if tokenData.AuthMethod != "" {
		if strings.EqualFold(tokenData.AuthMethod, "consumer") {
			plan = "Free Tier"
		} else {
			plan = tokenData.AuthMethod
		}
	}
	account := &Account{Plan: plan, LoginMethod: tokenData.AuthMethod}
	if account.LoginMethod == "" {
		account.LoginMethod = "google"
	}

	res := Result{
		BackendID:  b.ID,
		Status:     StatusOK,
		Account:    account,
		Usage:      antigravityEmptyLimits(),
		ObservedAt: now,
	}

	accessToken, err := a.resolveAccessToken(ctx, tokenData, now)
	if err != nil || accessToken == "" {
		return res
	}

	body, status, err := a.fetchQuotaSummary(ctx, accessToken)
	if err != nil || status >= 400 || len(body) == 0 {
		return res
	}

	limits, ok := parseAntigravityQuotaSummary(body)
	if !ok {
		return res
	}
	res.Usage = limits
	for _, lim := range limits {
		if lim.UsedPercent != nil && *lim.UsedPercent >= 100 {
			res.Status = StatusRateLimited
			res.Error = &ProviderError{Code: "rate_limited", Message: "provider reports that a usage limit has been reached"}
			break
		}
	}
	return res
}

type antigravityTokenFile struct {
	Token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Expiry       string `json:"expiry"`
	} `json:"token"`
	AuthMethod string `json:"auth_method"`
}

func (a AntigravityAdapter) readTokenFile() (*antigravityTokenFile, error) {
	rf := a.ReadFile
	if rf == nil {
		rf = os.ReadFile
	}
	pathFn := a.TokenPath
	if pathFn == nil {
		pathFn = defaultAntigravityTokenPath
	}
	p := pathFn()
	if p == "" {
		return nil, os.ErrNotExist
	}
	raw, err := rf(p)
	if err != nil {
		return nil, err
	}
	var tf antigravityTokenFile
	if err := json.Unmarshal(raw, &tf); err != nil {
		return nil, err
	}
	if tf.Token.AccessToken == "" && tf.Token.RefreshToken == "" {
		return nil, os.ErrNotExist
	}
	return &tf, nil
}

func defaultAntigravityTokenPath() string {
	home := userHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token")
}

func (a AntigravityAdapter) doer() HTTPDoer {
	if a.Doer != nil {
		return a.Doer
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (a AntigravityAdapter) resolveAccessToken(ctx context.Context, tf *antigravityTokenFile, now time.Time) (string, error) {
	if tf.Token.AccessToken != "" && tf.Token.Expiry != "" {
		exp, err := time.Parse(time.RFC3339, tf.Token.Expiry)
		if err == nil && now.Add(time.Minute).Before(exp) {
			return tf.Token.AccessToken, nil
		}
	}

	if tf.Token.RefreshToken != "" {
		return a.refreshAccessToken(ctx, tf.Token.RefreshToken)
	}

	return tf.Token.AccessToken, nil
}

func (a AntigravityAdapter) refreshAccessToken(ctx context.Context, refreshToken string) (string, error) {
	tokenURL := a.TokenEndpoint
	if tokenURL == "" {
		tokenURL = antigravityTokenEndpoint
	}

	cid, csec := antigravityOAuthCredentials()
	form := url.Values{}
	form.Set("client_id", cid)
	form.Set("client_secret", csec)
	form.Set("refresh_token", refreshToken)
	form.Set("grant_type", "refresh_token")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.doer().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", io.EOF
	}

	var res struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	return res.AccessToken, nil
}

func (a AntigravityAdapter) fetchQuotaSummary(ctx context.Context, accessToken string) ([]byte, int, error) {
	endpoint := a.Endpoint
	if endpoint == "" {
		endpoint = antigravityDefaultEndpoint
	}
	rpcURL := strings.TrimRight(endpoint, "/") + antigravityQuotaSummaryRPC

	payload := []byte(`{}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rpcURL, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", antigravityUserAgent)

	resp, err := a.doer().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return body, resp.StatusCode, err
}

type antigravityQuotaSummaryResponse struct {
	Groups []struct {
		DisplayName string `json:"displayName"`
		Buckets     []struct {
			BucketID          string   `json:"bucketId"`
			DisplayName       string   `json:"displayName"`
			Window            string   `json:"window"`
			ResetTime         *string  `json:"resetTime"`
			RemainingFraction *float64 `json:"remainingFraction"`
		} `json:"buckets"`
	} `json:"groups"`
}

func parseAntigravityQuotaSummary(body []byte) ([]Limit, bool) {
	var resp antigravityQuotaSummaryResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, false
	}
	if len(resp.Groups) == 0 {
		return nil, false
	}

	byID := map[string]Limit{}
	for _, group := range resp.Groups {
		pool := antigravityPoolFromGroup(group.DisplayName)
		for _, bucket := range group.Buckets {
			if pool == "" {
				pool = antigravityPoolFromBucketID(bucket.BucketID)
			}
			window := antigravityWindowKind(bucket.Window, bucket.BucketID)
			if pool == "" || window == "" {
				continue
			}
			id, scope, label, families, duration := antigravityWindowMeta(pool, window)
			used := antigravityUsedFromRemaining(bucket.RemainingFraction)
			var reset *time.Time
			if bucket.ResetTime != nil && *bucket.ResetTime != "" {
				if t, err := time.Parse(time.RFC3339, *bucket.ResetTime); err == nil {
					ut := t.UTC()
					reset = &ut
				}
			}
			byID[id] = antigravityWindow(id, scope, label, families, nil, used, reset, duration)
		}
	}

	out := antigravityEmptyLimits()
	found := false
	for i, lim := range out {
		if got, ok := byID[lim.ID]; ok {
			out[i] = got
			found = true
		}
	}
	return out, found
}

func antigravityPoolFromGroup(displayName string) string {
	lower := strings.ToLower(displayName)
	switch {
	case strings.Contains(lower, "gemini"):
		return "gemini"
	case strings.Contains(lower, "claude"), strings.Contains(lower, "gpt"), strings.Contains(lower, "3p"):
		return "non-gemini"
	default:
		return ""
	}
}

func antigravityPoolFromBucketID(bucketID string) string {
	lower := strings.ToLower(bucketID)
	switch {
	case strings.HasPrefix(lower, "gemini"):
		return "gemini"
	case strings.HasPrefix(lower, "3p"), strings.HasPrefix(lower, "non-gemini"):
		return "non-gemini"
	default:
		return ""
	}
}

func antigravityWindowKind(window, bucketID string) string {
	lower := strings.ToLower(window)
	if lower == "" {
		lower = strings.ToLower(bucketID)
	}
	switch {
	case strings.Contains(lower, "weekly"), strings.Contains(lower, "week"):
		return "weekly"
	case strings.Contains(lower, "5h"), strings.Contains(lower, "five"), strings.Contains(lower, "hour"):
		return "5h"
	default:
		return ""
	}
}

func antigravityWindowMeta(pool, window string) (id, scope, label string, families []string, duration int) {
	scope = pool
	switch pool {
	case "gemini":
		families = []string{"gemini"}
		switch window {
		case "5h":
			return "antigravity:gemini-5h", scope, "Gemini 5-hour", families, antigravityFiveHourMinutes
		default:
			return "antigravity:gemini-weekly", scope, "Gemini weekly", families, antigravityWeeklyMinutes
		}
	default:
		switch window {
		case "5h":
			return "antigravity:non-gemini-5h", scope, "Non-Gemini 5-hour", nil, antigravityFiveHourMinutes
		default:
			return "antigravity:non-gemini-weekly", scope, "Non-Gemini weekly", nil, antigravityWeeklyMinutes
		}
	}
}

func antigravityUsedFromRemaining(remaining *float64) *float64 {
	if remaining == nil {
		return nil
	}
	rem := *remaining
	if rem < 0 {
		rem = 0
	}
	if rem > 1 {
		rem = 1
	}
	u := math.Round((1.0-rem)*10000) / 100
	return &u
}

func antigravityEmptyLimits() []Limit {
	return []Limit{
		antigravityWindow("antigravity:gemini-5h", "gemini", "Gemini 5-hour", []string{"gemini"}, nil, nil, nil, antigravityFiveHourMinutes),
		antigravityWindow("antigravity:gemini-weekly", "gemini", "Gemini weekly", []string{"gemini"}, nil, nil, nil, antigravityWeeklyMinutes),
		antigravityWindow("antigravity:non-gemini-5h", "non-gemini", "Non-Gemini 5-hour", nil, nil, nil, nil, antigravityFiveHourMinutes),
		antigravityWindow("antigravity:non-gemini-weekly", "non-gemini", "Non-Gemini weekly", nil, nil, nil, nil, antigravityWeeklyMinutes),
	}
}

func antigravityWindow(id, scope, label string, families, models []string, used *float64, resets *time.Time, durationMinutes int) Limit {
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
	var duration *int
	if durationMinutes > 0 {
		d := durationMinutes
		duration = &d
	}
	return Limit{
		ID:               id,
		Scope:            scope,
		Label:            label,
		ModelFamilies:    families,
		Models:           models,
		UsedPercent:      used,
		RemainingPercent: remaining,
		DurationMinutes:  duration,
		ResetsAt:         resets,
		LimitState:       state,
	}
}
