package backendusage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"sort"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
)

const maxRPCMessage = 1 << 20

type CodexAdapter struct {
	Now   func() time.Time
	Start func(context.Context, string) (io.WriteCloser, io.ReadCloser, func() error, error)
}

func (a CodexAdapter) BackendID() string { return "codex" }

func (a CodexAdapter) Fetch(ctx context.Context, b backendstore.Backend) Result {
	now := clock(a.Now)
	if !b.Installed {
		return notInstalled(b.ID, now)
	}
	start := a.Start
	if start == nil {
		start = startCodex
	}
	in, out, wait, err := start(ctx, binary(b, "codex"))
	if err != nil {
		return commandFailure(b.ID, ctx, now)
	}
	defer func() {
		_ = in.Close()
		_ = out.Close()
		_ = wait()
	}()
	enc := json.NewEncoder(in)
	scan := bufio.NewScanner(out)
	scan.Buffer(make([]byte, 64*1024), maxRPCMessage)
	call := func(id int, method string, params any, dst any) error {
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
			return err
		}
		for scan.Scan() {
			var env struct {
				ID     json.RawMessage `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(scan.Bytes(), &env) != nil || len(env.ID) == 0 {
				continue
			}
			var got int
			if json.Unmarshal(env.ID, &got) != nil || got != id {
				continue
			}
			if len(env.Error) > 0 && string(env.Error) != "null" {
				return errors.New("rpc error")
			}
			return json.Unmarshal(env.Result, dst)
		}
		if err := scan.Err(); err != nil {
			return err
		}
		return io.ErrUnexpectedEOF
	}
	var initialized json.RawMessage
	if err := call(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "warden", "version": "1"}, "capabilities": map[string]any{}}, &initialized); err != nil {
		return commandFailureOrMalformed(b.ID, ctx, now, err)
	}
	if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}}); err != nil {
		return commandFailure(b.ID, ctx, now)
	}
	var accountResp struct {
		Account *struct {
			Type     string `json:"type"`
			PlanType string `json:"planType"`
		} `json:"account"`
		RequiresOpenAIAuth bool `json:"requiresOpenaiAuth"`
	}
	if err := call(2, "account/read", map[string]any{"refreshToken": false}, &accountResp); err != nil {
		return commandFailureOrMalformed(b.ID, ctx, now, err)
	}
	if accountResp.Account == nil && accountResp.RequiresOpenAIAuth {
		return unauthenticated(b.ID, now)
	}
	var rates codexRateResponse
	if err := call(3, "account/rateLimits/read", map[string]any{}, &rates); err != nil {
		return commandFailureOrMalformed(b.ID, ctx, now, err)
	}
	res := Result{BackendID: b.ID, Status: StatusOK, Windows: []Window{}, ObservedAt: now}
	if accountResp.Account != nil {
		res.Account = &Account{Plan: accountResp.Account.PlanType, LoginMethod: accountResp.Account.Type}
	}
	snaps := rates.RateLimitsByLimitID
	if len(snaps) == 0 && rates.RateLimits != nil {
		snaps = map[string]codexRateSnapshot{"codex": *rates.RateLimits}
	}
	ids := make([]string, 0, len(snaps))
	for id := range snaps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := snaps[id]
		if res.Account != nil && res.Account.Plan == "" {
			res.Account.Plan = s.PlanType
		}
		for _, pair := range []struct {
			name string
			w    *codexWindow
		}{{"primary", s.Primary}, {"secondary", s.Secondary}} {
			if pair.w == nil {
				continue
			}
			wid := id + ":" + pair.name
			used := pair.w.UsedPercent
			var remaining *float64
			if used != nil && *used >= 0 && *used <= 100 {
				v := 100 - *used
				remaining = &v
			}
			var reset *time.Time
			if pair.w.ResetsAt != nil {
				v := time.Unix(*pair.w.ResetsAt, 0).UTC()
				reset = &v
			}
			res.Windows = append(res.Windows, Window{ID: wid, UsedPercent: used, RemainingPercent: remaining, DurationMinutes: pair.w.WindowDurationMins, ResetsAt: reset, LimitState: s.limitState()})
		}
		if s.reached() {
			res.Status = StatusRateLimited
			res.Error = &ProviderError{Code: "rate_limited", Message: "provider reports that a usage limit has been reached"}
		}
	}
	return res
}

type codexRateResponse struct {
	RateLimits          *codexRateSnapshot           `json:"rateLimits"`
	RateLimitsByLimitID map[string]codexRateSnapshot `json:"rateLimitsByLimitId"`
}
type codexWindow struct {
	UsedPercent        *float64 `json:"usedPercent"`
	WindowDurationMins *int     `json:"windowDurationMins"`
	ResetsAt           *int64   `json:"resetsAt"`
}
type codexRateSnapshot struct {
	PlanType     string       `json:"planType"`
	Primary      *codexWindow `json:"primary"`
	Secondary    *codexWindow `json:"secondary"`
	LimitReached bool         `json:"limitReached"`
	Credits      *struct {
		HasCredits bool            `json:"hasCredits"`
		Unlimited  bool            `json:"unlimited"`
		Balance    json.RawMessage `json:"balance"`
	} `json:"credits"`
}

func (s codexRateSnapshot) reached() bool {
	return s.LimitReached || (s.Credits != nil && !s.Credits.HasCredits && !s.Credits.Unlimited)
}
func (s codexRateSnapshot) limitState() *string {
	if !s.reached() {
		return nil
	}
	v := "reached"
	return &v
}

func startCodex(ctx context.Context, binary string) (io.WriteCloser, io.ReadCloser, func() error, error) {
	cmd := exec.CommandContext(ctx, binary, "app-server", "--listen", "stdio://")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		in.Close()
		out.Close()
		return nil, nil, nil, err
	}
	return in, out, cmd.Wait, nil
}

func commandFailureOrMalformed(id string, ctx context.Context, now time.Time, err error) Result {
	if ctx.Err() != nil {
		return commandFailure(id, ctx, now)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return malformed(id, now)
	}
	return malformed(id, now)
}
