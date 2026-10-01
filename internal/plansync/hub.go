package plansync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/planstore"
)

// Frozen Hub plan-sync HTTP paths (Phase A client contract; Phase B Hub service
// implements these). See docs/specs/2026-09-30-plan-hub-sync-boundary.md.
const (
	PathPush     = "/api/v1/plan-sync/push"
	PathPull     = "/api/v1/plan-sync/pull"
	PathDiscover = "/api/v1/plan-sync/discover"
)

const (
	defaultHubHTTPTimeout = 30 * time.Second
	maxHubErrorBody       = 4 << 10
)

// HubOptions configures a network-backed PlanSyncProvider.
type HubOptions struct {
	// BaseURL is the warden-hub origin (e.g. https://hub.example.com). Required.
	BaseURL string
	// Token is the bearer credential sent as Authorization: Bearer …. Required
	// for Enabled() to return true; Push/Pull/Discover return ErrDisabled when empty.
	Token string
	// HTTP is the client used for Hub calls; nil ⇒ a client with a 30s timeout.
	HTTP *http.Client
	// Now supplies synced_at when the Hub response omits it; nil ⇒ time.Now.
	Now func() time.Time
}

// HubProvider is the remote PlanSyncProvider (Name=hub). It speaks the frozen
// envelope protocol over HTTP against a configurable Hub base URL + bearer token.
// SyncedAt / RemoteID are stamped onto planstore.Plan only via StampPlan /
// SyncPushPlan / SyncPullPlan after a successful Hub round-trip — never by
// Local, Fake, or planexport.
type HubProvider struct {
	baseURL string
	token   string
	http    *http.Client
	now     func() time.Time
}

// NewHub builds a HubProvider. baseURL is required; a missing/empty token leaves
// the provider constructed but Disabled (methods return ErrDisabled).
func NewHub(opts HubOptions) (*HubProvider, error) {
	base := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("plansync: hub base URL is required")
	}
	client := opts.HTTP
	if client == nil {
		client = &http.Client{Timeout: defaultHubHTTPTimeout}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &HubProvider{
		baseURL: base,
		token:   strings.TrimSpace(opts.Token),
		http:    client,
		now:     now,
	}, nil
}

// Name implements PlanSyncProvider.
func (*HubProvider) Name() string { return ProviderHub }

// Enabled implements PlanSyncProvider. True only when base URL and token are set.
func (h *HubProvider) Enabled() bool {
	return h != nil && h.baseURL != "" && h.token != ""
}

// Push implements PlanSyncProvider. On success the Hub response's remote_id /
// synced_at are discarded here — use SyncPushPlan (or PushEnvelope + StampPlan)
// to stamp a local Plan.
func (h *HubProvider) Push(ctx context.Context, env Envelope) error {
	_, err := h.PushEnvelope(ctx, env)
	return err
}

// PushEnvelope POSTs env to the Hub and returns the Hub-acknowledged envelope
// (with remote_id / synced_at filled). Does not mutate any planstore.Plan.
func (h *HubProvider) PushEnvelope(ctx context.Context, env Envelope) (Envelope, error) {
	if err := h.requireEnabled(); err != nil {
		return Envelope{}, err
	}
	if err := validateEnvelope(env); err != nil {
		return Envelope{}, err
	}

	var out Envelope
	if err := h.doJSON(ctx, http.MethodPost, PathPush, env, &out); err != nil {
		var cerr *ConflictError
		if errors.As(err, &cerr) && cerr.PlanID == "" {
			cerr.PlanID = env.PlanID
		}
		return Envelope{}, err
	}
	out = fillSyncedAt(out, h.now())
	if out.RemoteID == "" {
		return Envelope{}, fmt.Errorf("plansync: hub push response missing remote_id")
	}
	if err := validateEnvelope(out); err != nil {
		return Envelope{}, fmt.Errorf("plansync: hub push response: %w", err)
	}
	return out, nil
}

// Pull implements PlanSyncProvider.
func (h *HubProvider) Pull(ctx context.Context, q PullQuery) ([]Envelope, error) {
	if err := h.requireEnabled(); err != nil {
		return nil, err
	}
	var resp envelopesResponse
	if err := h.doJSON(ctx, http.MethodPost, PathPull, pullRequest{
		Scope:    q.Scope,
		PlanID:   q.PlanID,
		Statuses: q.Statuses,
	}, &resp); err != nil {
		return nil, err
	}
	return fillSyncedAtAll(resp.Envelopes, h.now()), nil
}

// Discover implements PlanSyncProvider. Empty statuses defaults to pending +
// in_progress (team-discovery protocol).
func (h *HubProvider) Discover(ctx context.Context, scope Scope, statuses []planstore.PlanStatus) ([]Envelope, error) {
	if err := h.requireEnabled(); err != nil {
		return nil, err
	}
	if len(statuses) == 0 {
		statuses = []planstore.PlanStatus{planstore.PlanStatusPending, planstore.PlanStatusInProgress}
	}
	var resp envelopesResponse
	if err := h.doJSON(ctx, http.MethodPost, PathDiscover, discoverRequest{
		Scope:    scope,
		Statuses: statuses,
	}, &resp); err != nil {
		return nil, err
	}
	return fillSyncedAtAll(resp.Envelopes, h.now()), nil
}

// SyncPushPlan pushes p's current revision and stamps Plan.SyncedAt / RemoteID
// only after the Hub acknowledges success.
func (h *HubProvider) SyncPushPlan(ctx context.Context, p *planstore.Plan, opts EnvelopeOptions) error {
	if p == nil {
		return fmt.Errorf("plansync: plan is nil")
	}
	env, err := EnvelopeFromPlan(p, opts)
	if err != nil {
		return err
	}
	out, err := h.PushEnvelope(ctx, env)
	if err != nil {
		return err
	}
	if out.SyncedAt == nil {
		return fmt.Errorf("plansync: hub push response missing synced_at")
	}
	StampPlan(p, out.RemoteID, *out.SyncedAt)
	return nil
}

// SyncPullPlan pulls the Hub revision for p.ID (scoped) and stamps
// Plan.SyncedAt / RemoteID only after a successful match. Returns
// ErrDisabled when the provider is off; fmt error when no envelope matches.
func (h *HubProvider) SyncPullPlan(ctx context.Context, p *planstore.Plan, scope Scope) error {
	if p == nil {
		return fmt.Errorf("plansync: plan is nil")
	}
	if strings.TrimSpace(p.ID) == "" {
		return errMissing("plan_id")
	}
	got, err := h.Pull(ctx, PullQuery{Scope: scope, PlanID: p.ID})
	if err != nil {
		return err
	}
	if len(got) == 0 {
		return fmt.Errorf("plansync: hub pull returned no envelope for plan %s", p.ID)
	}
	env := got[0]
	if env.RemoteID == "" || env.SyncedAt == nil {
		return fmt.Errorf("plansync: hub pull response missing remote_id/synced_at for plan %s", p.ID)
	}
	StampPlan(p, env.RemoteID, *env.SyncedAt)
	return nil
}

func (h *HubProvider) requireEnabled() error {
	if h == nil || !h.Enabled() {
		return ErrDisabled
	}
	return nil
}

type pullRequest struct {
	Scope    Scope                  `json:"scope"`
	PlanID   string                 `json:"plan_id,omitempty"`
	Statuses []planstore.PlanStatus `json:"statuses,omitempty"`
}

type discoverRequest struct {
	Scope    Scope                  `json:"scope"`
	Statuses []planstore.PlanStatus `json:"statuses,omitempty"`
}

type envelopesResponse struct {
	Envelopes []Envelope `json:"envelopes"`
}

type hubErrorBody struct {
	Error    string `json:"error"`
	PlanID   string `json:"plan_id,omitempty"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
	Message  string `json:"message,omitempty"`
}

func (h *HubProvider) doJSON(ctx context.Context, method, path string, reqBody any, respBody any) error {
	var body io.Reader
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("plansync: marshal request: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("plansync: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+h.token)

	res, err := h.http.Do(req)
	if err != nil {
		return fmt.Errorf("plansync: hub request %s %s: %w", method, path, err)
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(res.Body, maxHubErrorBody+1))
	if err != nil {
		return fmt.Errorf("plansync: read hub response: %w", err)
	}
	if len(raw) > maxHubErrorBody {
		return fmt.Errorf("plansync: hub response too large")
	}

	if res.StatusCode == http.StatusConflict {
		var eb hubErrorBody
		_ = json.Unmarshal(raw, &eb)
		return &ConflictError{
			PlanID:   eb.PlanID,
			Expected: eb.Expected,
			Actual:   eb.Actual,
		}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var eb hubErrorBody
		_ = json.Unmarshal(raw, &eb)
		msg := strings.TrimSpace(eb.Message)
		if msg == "" {
			msg = strings.TrimSpace(eb.Error)
		}
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		if msg == "" {
			msg = res.Status
		}
		return fmt.Errorf("plansync: hub %s %s: HTTP %d: %s", method, path, res.StatusCode, msg)
	}
	if respBody == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, respBody); err != nil {
		return fmt.Errorf("plansync: decode hub response: %w", err)
	}
	return nil
}

// fillSyncedAt sets SyncedAt when the Hub omitted it on an otherwise successful
// response. RemoteID is never invented here — Phase B Hub must assign it.
func fillSyncedAt(env Envelope, now time.Time) Envelope {
	if env.SyncedAt == nil {
		t := now.UTC()
		env.SyncedAt = &t
	} else {
		t := env.SyncedAt.UTC()
		env.SyncedAt = &t
	}
	return env
}

func fillSyncedAtAll(in []Envelope, now time.Time) []Envelope {
	if len(in) == 0 {
		return nil
	}
	out := make([]Envelope, len(in))
	for i, env := range in {
		out[i] = fillSyncedAt(env, now)
	}
	return out
}
