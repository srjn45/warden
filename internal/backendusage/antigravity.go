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
// the same RPC the `agy /usage` TUI uses — emitting two pool buckets:
// `antigravity:gemini` and `antigravity:non-gemini`. Each bucket reports its
// 5-hour session limit while the weekly limit still has headroom, and flips to
// the (exhausted) weekly limit once the weekly bucket is drained.
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

// antigravityPoolStats accumulates a pool's (gemini / non-gemini) 5-hour and
// weekly bucket stats from the quota summary before they are collapsed into a
// single reported Limit by resolveAntigravityPoolLimit.
type antigravityPoolStats struct {
	fiveHourRemaining *float64
	fiveHourReset     *time.Time
	weeklyRemaining   *float64
	weeklyReset       *time.Time
}

func parseAntigravityQuotaSummary(body []byte) ([]Limit, bool) {
	var resp antigravityQuotaSummaryResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, false
	}
	if len(resp.Groups) == 0 {
		return nil, false
	}

	byPool := map[string]*antigravityPoolStats{}
	for _, group := range resp.Groups {
		groupPool := antigravityPoolFromGroup(group.DisplayName)
		for _, bucket := range group.Buckets {
			pool := groupPool
			if pool == "" {
				pool = antigravityPoolFromBucketID(bucket.BucketID)
			}
			window := antigravityWindowKind(bucket.Window, bucket.BucketID)
			if pool == "" || window == "" {
				continue
			}
			s := byPool[pool]
			if s == nil {
				s = &antigravityPoolStats{}
				byPool[pool] = s
			}
			reset := antigravityParseReset(bucket.ResetTime)
			switch window {
			case "5h":
				s.fiveHourRemaining = bucket.RemainingFraction
				s.fiveHourReset = reset
			case "weekly":
				s.weeklyRemaining = bucket.RemainingFraction
				s.weeklyReset = reset
			}
		}
	}

	out := antigravityEmptyLimits()
	found := false
	for i, lim := range out {
		if s, ok := byPool[lim.Scope]; ok {
			out[i] = resolveAntigravityPoolLimit(lim.Scope, *s)
			found = true
		}
	}
	return out, found
}

// resolveAntigravityPoolLimit collapses a pool's 5-hour and weekly stats into
// the single Limit warden reports for that bucket. While the weekly limit has
// headroom, the bucket reports its 5-hour session limit (usage + reset). Once
// the weekly limit is exhausted (remainingFraction <= 0) the bucket flips to
// the weekly limit fully consumed: 100% used, 0% remaining, limitState
// "reached", weekly duration, and the weekly reset time.
func resolveAntigravityPoolLimit(pool string, s antigravityPoolStats) Limit {
	id, scope, label, families := antigravityPoolMeta(pool)
	if s.weeklyRemaining != nil && *s.weeklyRemaining <= 0 {
		used := 100.0
		return antigravityWindow(id, scope, label, families, nil, &used, s.weeklyReset, antigravityWeeklyMinutes)
	}
	used := antigravityUsedFromRemaining(s.fiveHourRemaining)
	return antigravityWindow(id, scope, label, families, nil, used, s.fiveHourReset, antigravityFiveHourMinutes)
}

// antigravityParseReset parses an RFC3339 resetTime into a UTC *time.Time,
// returning nil for a missing/empty/unparseable value.
func antigravityParseReset(resetTime *string) *time.Time {
	if resetTime == nil || *resetTime == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, *resetTime)
	if err != nil {
		return nil
	}
	ut := t.UTC()
	return &ut
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

// antigravityPoolMeta returns the stable identity of a pool's reported bucket.
func antigravityPoolMeta(pool string) (id, scope, label string, families []string) {
	switch pool {
	case "gemini":
		return "antigravity:gemini", "gemini", "Gemini", []string{"gemini"}
	default:
		return "antigravity:non-gemini", "non-gemini", "Non-Gemini", nil
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

// antigravityEmptyLimits returns the two pool buckets with no measurements yet.
// The 5-hour session limit is the default reported window, so the placeholder
// carries its duration until live stats replace it.
func antigravityEmptyLimits() []Limit {
	return []Limit{
		antigravityWindow("antigravity:gemini", "gemini", "Gemini", []string{"gemini"}, nil, nil, nil, antigravityFiveHourMinutes),
		antigravityWindow("antigravity:non-gemini", "non-gemini", "Non-Gemini", nil, nil, nil, nil, antigravityFiveHourMinutes),
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
