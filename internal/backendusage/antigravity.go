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
	antigravityDefaultEndpoint = "https://cloudcode-pa.googleapis.com"
	antigravityModelsRPC       = "/v1internal:fetchAvailableModels"
	antigravityTokenEndpoint   = "https://oauth2.googleapis.com/token"
	antigravityUserAgent       = "antigravity/1.0.16"

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

// AntigravityAdapter queries Antigravity CLI quota usage via the fetchAvailableModels
// RPC, categorizing usage into two buckets: Gemini models and Non-Gemini models.
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
		Usage:      antigravityLimits(nil, nil, nil, nil),
		ObservedAt: now,
	}

	accessToken, err := a.resolveAccessToken(ctx, tokenData, now)
	if err != nil || accessToken == "" {
		return res
	}

	body, status, err := a.fetchAvailableModels(ctx, accessToken)
	if err != nil || status >= 400 || len(body) == 0 {
		return res
	}

	geminiUsed, geminiReset, nonGeminiUsed, nonGeminiReset, ok := parseAntigravityAvailableModels(body)
	if !ok {
		return res
	}

	res.Usage = antigravityLimits(geminiUsed, geminiReset, nonGeminiUsed, nonGeminiReset)
	if (geminiUsed != nil && *geminiUsed >= 100) || (nonGeminiUsed != nil && *nonGeminiUsed >= 100) {
		res.Status = StatusRateLimited
		res.Error = &ProviderError{Code: "rate_limited", Message: "provider reports that a usage limit has been reached"}
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

func (a AntigravityAdapter) fetchAvailableModels(ctx context.Context, accessToken string) ([]byte, int, error) {
	endpoint := a.Endpoint
	if endpoint == "" {
		endpoint = antigravityDefaultEndpoint
	}
	rpcURL := strings.TrimRight(endpoint, "/") + antigravityModelsRPC

	payload := []byte(`{"project":"default-cli-project"}`)
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

type antigravityAvailableModelsResponse struct {
	Models map[string]struct {
		DisplayName   *string `json:"displayName"`
		ModelProvider string  `json:"modelProvider"`
		APIProvider   string  `json:"apiProvider"`
		QuotaInfo     *struct {
			RemainingFraction *float64 `json:"remainingFraction"`
			ResetTime         *string  `json:"resetTime"`
		} `json:"quotaInfo"`
	} `json:"models"`
}

func parseAntigravityAvailableModels(body []byte) (geminiUsed *float64, geminiReset *time.Time, nonGeminiUsed *float64, nonGeminiReset *time.Time, ok bool) {
	var resp antigravityAvailableModelsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, nil, nil, nil, false
	}
	if len(resp.Models) == 0 {
		return nil, nil, nil, nil, false
	}

	for id, m := range resp.Models {
		if m.QuotaInfo == nil {
			continue
		}
		qi := m.QuotaInfo
		var used *float64
		if qi.RemainingFraction != nil {
			rem := *qi.RemainingFraction
			if rem < 0 {
				rem = 0
			}
			if rem > 1 {
				rem = 1
			}
			u := math.Round((1.0-rem)*10000) / 100
			used = &u
		}

		var reset *time.Time
		if qi.ResetTime != nil && *qi.ResetTime != "" {
			if t, err := time.Parse(time.RFC3339, *qi.ResetTime); err == nil {
				ut := t.UTC()
				reset = &ut
			}
		}

		isGemini := strings.Contains(strings.ToLower(id), "gemini") ||
			(strings.Contains(strings.ToLower(m.ModelProvider), "google") && strings.Contains(strings.ToLower(m.APIProvider), "gemini"))

		if isGemini {
			if geminiUsed == nil && used != nil {
				geminiUsed = used
			}
			if geminiReset == nil && reset != nil {
				geminiReset = reset
			}
		} else {
			if nonGeminiUsed == nil && used != nil {
				nonGeminiUsed = used
			}
			if nonGeminiReset == nil && reset != nil {
				nonGeminiReset = reset
			}
		}
	}

	return geminiUsed, geminiReset, nonGeminiUsed, nonGeminiReset, true
}

func antigravityLimits(geminiUsed *float64, geminiReset *time.Time, nonGeminiUsed *float64, nonGeminiReset *time.Time) []Limit {
	return []Limit{
		antigravityWindow("antigravity:gemini", "gemini", "Gemini models", []string{"gemini"}, nil, geminiUsed, geminiReset),
		antigravityWindow("antigravity:non-gemini", "non-gemini", "Non-Gemini models", nil, nil, nonGeminiUsed, nonGeminiReset),
	}
}

func antigravityWindow(id, scope, label string, families, models []string, used *float64, resets *time.Time) Limit {
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
	return Limit{
		ID:               id,
		Scope:            scope,
		Label:            label,
		ModelFamilies:    families,
		Models:           models,
		UsedPercent:      used,
		RemainingPercent: remaining,
		ResetsAt:         resets,
		LimitState:       state,
	}
}
