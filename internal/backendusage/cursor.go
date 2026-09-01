package backendusage

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
)

const (
	cursorDefaultEndpoint = "https://api2.cursor.sh"
	cursorUsageRPC        = "/aiserver.v1.DashboardService/GetCurrentPeriodUsage"
	cursorPlanRPC         = "/aiserver.v1.DashboardService/GetPlanInfo"
)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// CursorAdapter reads Cursor's three subscription windows from the dashboard
// Connect-RPC used by the CLI usage pager (not a cursor-agent subcommand).
type CursorAdapter struct {
	Runner   CommandRunner
	Now      func() time.Time
	ReadFile func(string) ([]byte, error)
	AuthPath func() string
	Doer     HTTPDoer
	Endpoint string
}

func (a CursorAdapter) BackendID() string { return "cursor" }

func (a CursorAdapter) Fetch(ctx context.Context, b backendstore.Backend) Result {
	now := clock(a.Now)
	if !b.Installed {
		return notInstalled(b.ID, now)
	}

	token := a.accessToken()
	type statusCall struct {
		out []byte
		err error
	}
	type usageCall struct {
		body   []byte
		status int
		plan   string
		err    error
	}
	statusCh := make(chan statusCall, 1)
	usageCh := make(chan usageCall, 1)
	go func() {
		out, err := a.runner().Output(ctx, binary(b, "cursor-agent"), "status", "--format", "json")
		statusCh <- statusCall{out, err}
	}()
	go func() {
		if token == "" {
			usageCh <- usageCall{}
			return
		}
		body, code, plan, err := a.fetchUsageAndPlan(ctx, token)
		usageCh <- usageCall{body, code, plan, err}
	}()
	st := <-statusCh
	us := <-usageCh

	var status struct {
		IsAuthenticated bool `json:"isAuthenticated"`
	}
	decoded := json.Unmarshal(st.out, &status) == nil
	if st.err != nil {
		if decoded && !status.IsAuthenticated {
			return unauthenticated(b.ID, now)
		}
		return commandFailure(b.ID, ctx, now)
	}
	if !decoded {
		return malformed(b.ID, now)
	}
	if !status.IsAuthenticated {
		return unauthenticated(b.ID, now)
	}

	account := &Account{Plan: us.plan, LoginMethod: "cursor"}
	if account.Plan == "" {
		account.Plan = a.planFromAbout(ctx, b)
	}

	res := Result{BackendID: b.ID, Status: StatusOK, Account: account, Usage: cursorLimits(nil, nil, nil, nil), ObservedAt: now}
	if token == "" {
		return res
	}
	if us.status == http.StatusUnauthorized {
		res.Status = StatusUnavailable
		res.Error = &ProviderError{Code: "unavailable", Message: "backend usage probe is unavailable"}
		return res
	}
	if us.err != nil || us.status >= 400 || len(us.body) == 0 {
		return res
	}
	parsed, ok := parseCursorPeriodUsage(us.body)
	if !ok {
		return res
	}
	res.Usage = parsed.limits
	if parsed.rateLimited {
		res.Status = StatusRateLimited
		res.Error = &ProviderError{Code: "rate_limited", Message: "provider reports that a usage limit has been reached"}
	}
	return res
}

func (a CursorAdapter) runner() CommandRunner {
	if a.Runner != nil {
		return a.Runner
	}
	return execRunner{}
}

func (a CursorAdapter) accessToken() string {
	read := a.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	path := ""
	if a.AuthPath != nil {
		path = a.AuthPath()
	} else {
		path = defaultCursorAuthPath()
	}
	if path == "" {
		return ""
	}
	raw, err := read(path)
	if err != nil || len(raw) == 0 {
		return ""
	}
	var auth struct {
		AccessToken string `json:"accessToken"`
	}
	if json.Unmarshal(raw, &auth) != nil {
		return ""
	}
	return strings.TrimSpace(auth.AccessToken)
}

func defaultCursorAuthPath() string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("APPDATA"), "Cursor", "auth.json")
	case "darwin":
		return filepath.Join(userHomeDir(), ".cursor", "auth.json")
	default:
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			return filepath.Join(xdg, "cursor", "auth.json")
		}
		return filepath.Join(userHomeDir(), ".config", "cursor", "auth.json")
	}
}

func userHomeDir() string {
	home, _ := os.UserHomeDir()
	return home
}

func (a CursorAdapter) endpoint() string {
	if a.Endpoint != "" {
		return strings.TrimRight(a.Endpoint, "/")
	}
	if v := strings.TrimSpace(os.Getenv("CURSOR_API_ENDPOINT")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return cursorDefaultEndpoint
}

func (a CursorAdapter) doer() HTTPDoer {
	if a.Doer != nil {
		return a.Doer
	}
	return http.DefaultClient
}

func (a CursorAdapter) fetchUsageAndPlan(ctx context.Context, token string) (body []byte, status int, plan string, err error) {
	type rpc struct {
		body   []byte
		status int
		err    error
	}
	usageCh := make(chan rpc, 1)
	planCh := make(chan rpc, 1)
	go func() { usageCh <- a.postRPC(ctx, cursorUsageRPC, token) }()
	go func() { planCh <- a.postRPC(ctx, cursorPlanRPC, token) }()
	u := <-usageCh
	p := <-planCh
	return u.body, u.status, parseCursorPlanName(p.body), u.err
}

func (a CursorAdapter) postRPC(ctx context.Context, path, token string) struct {
	body   []byte
	status int
	err    error
} {
	var out struct {
		body   []byte
		status int
		err    error
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint()+path, bytes.NewReader([]byte("{}")))
	if err != nil {
		out.err = err
		return out
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	resp, err := a.doer().Do(req)
	if err != nil {
		out.err = err
		return out
	}
	defer resp.Body.Close()
	out.status = resp.StatusCode
	out.body, out.err = io.ReadAll(io.LimitReader(resp.Body, maxRPCMessage))
	return out
}

func (a CursorAdapter) planFromAbout(ctx context.Context, b backendstore.Backend) string {
	out, err := a.runner().Output(ctx, binary(b, "cursor-agent"), "about", "--format", "json")
	if err != nil {
		return ""
	}
	var about struct {
		SubscriptionTier string `json:"subscriptionTier"`
	}
	if json.Unmarshal(out, &about) != nil {
		return ""
	}
	return about.SubscriptionTier
}

type cursorPeriodParse struct {
	limits      []Limit
	rateLimited bool
}

func parseCursorPeriodUsage(body []byte) (cursorPeriodParse, bool) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return cursorPeriodParse{}, false
	}
	plan := nestedObject(root, "planUsage", "plan_usage")
	included := firstFloat(plan, "totalPercentUsed", "total_percent_used")
	auto := firstFloat(plan, "autoPercentUsed", "auto_percent_used")
	api := firstFloat(plan, "apiPercentUsed", "api_percent_used")
	resets := parseMillisTime(root, "billingCycleEnd", "billing_cycle_end")
	limits := cursorLimits(included, auto, api, resets)
	rateLimited := false
	for _, l := range limits {
		if l.LimitState != nil && *l.LimitState == "reached" {
			rateLimited = true
			break
		}
	}
	return cursorPeriodParse{limits: limits, rateLimited: rateLimited}, true
}

func cursorLimits(included, auto, api *float64, resets *time.Time) []Limit {
	return []Limit{
		cursorWindow("cursor:included", "included", "Included", []string{"composer", "cursor-grok"}, nil, included, resets),
		cursorWindow("cursor:auto", "auto", "Auto", nil, []string{"auto"}, auto, resets),
		cursorWindow("cursor:api", "api", "API", []string{"claude", "gpt", "gemini", "kimi", "glm"}, nil, api, resets),
	}
}

func cursorWindow(id, scope, label string, families, models []string, used *float64, resets *time.Time) Limit {
	var remaining *float64
	if used != nil && *used >= 0 && *used <= 100 {
		v := 100 - *used
		remaining = &v
	}
	var state *string
	if used != nil && *used >= 100 {
		v := "reached"
		state = &v
	}
	return Limit{
		ID: id, Scope: scope, Label: label,
		ModelFamilies: families, Models: models,
		UsedPercent: used, RemainingPercent: remaining,
		ResetsAt: resets, LimitState: state,
	}
}

func nestedObject(root map[string]json.RawMessage, keys ...string) map[string]json.RawMessage {
	for _, k := range keys {
		raw, ok := root[k]
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(raw, &nested) == nil {
			return nested
		}
	}
	return map[string]json.RawMessage{}
}

func firstFloat(m map[string]json.RawMessage, keys ...string) *float64 {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var f float64
		if json.Unmarshal(raw, &f) == nil {
			v := f
			return &v
		}
	}
	return nil
}

func parseMillisTime(root map[string]json.RawMessage, keys ...string) *time.Time {
	for _, k := range keys {
		raw, ok := root[k]
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}
		if t, ok := millisToTime(raw); ok {
			return t
		}
	}
	return nil
}

func millisToTime(raw json.RawMessage) (*time.Time, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, false
	}
	var n int64
	switch {
	case raw[0] == '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil, false
		}
		parsed, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, false
		}
		n = parsed
	default:
		if json.Unmarshal(raw, &n) != nil {
			var f float64
			if json.Unmarshal(raw, &f) != nil {
				return nil, false
			}
			n = int64(f)
		}
	}
	t := time.UnixMilli(n).UTC()
	return &t, true
}

func parseCursorPlanName(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return ""
	}
	if v := jsonString(root, "planName", "plan_name"); v != "" {
		return v
	}
	info := nestedObject(root, "planInfo", "plan_info")
	return jsonString(info, "planName", "plan_name")
}

func jsonString(m map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return s
		}
	}
	return ""
}
