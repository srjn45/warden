package backendusage

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/backendstore"
)

const (
	ProviderTimeout = 3 * time.Second
	RequestTimeout  = 5 * time.Second
	FreshTTL        = 60 * time.Second
	StaleTTL        = 15 * time.Minute
)

type Registry interface {
	List() ([]backendstore.Backend, error)
}
type cacheEntry struct {
	result     Result
	storedAt   time.Time
	installed  bool
	binaryPath string
}

type Service struct {
	registry Registry
	adapters map[string]Adapter
	now      func() time.Time
	mu       sync.Mutex
	cache    map[string]cacheEntry
}

func NewService(reg Registry, adapters ...Adapter) *Service {
	s := &Service{registry: reg, now: time.Now, cache: make(map[string]cacheEntry), adapters: make(map[string]Adapter)}
	if len(adapters) == 0 {
		adapters = []Adapter{CodexAdapter{}, ClaudeAdapter{}, CursorAdapter{}, AntigravityAdapter{}}
	}
	for _, a := range adapters {
		s.adapters[a.BackendID()] = a
	}
	return s
}

func (s *Service) Snapshot(ctx context.Context, refresh bool) (Snapshot, error) {
	if s == nil || s.registry == nil {
		return Snapshot{}, fmt.Errorf("backend registry unavailable")
	}
	rows, err := s.registry.List()
	if err != nil {
		return Snapshot{}, fmt.Errorf("list backend registry: %w", err)
	}
	selected := rows[:0]
	for _, b := range rows {
		if b.Tier == backendstore.TierSubscription {
			selected = append(selected, b)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].ID < selected[j].ID })
	now := s.now().UTC()
	snap := Snapshot{SchemaVersion: 1, GeneratedAt: now, Backends: make([]BackendResult, len(selected))}
	requestCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, b := range selected {
		i, b := i, b
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			snap.Backends[i] = s.collect(requestCtx, b, refresh, now)
		}()
	}
	wg.Wait()
	return snap, nil
}

func (s *Service) collect(ctx context.Context, b backendstore.Backend, refresh bool, now time.Time) BackendResult {
	if !refresh {
		if ce, ok := s.cached(b); ok && now.Sub(ce.storedAt) < FreshTTL {
			return project(b, ce.result, true, false, nil)
		}
	}
	a := s.adapters[b.ID]
	if a == nil {
		if ab, err := agentbackend.Get(b.ID); err == nil {
			if ul, ok := ab.(agentbackend.UsageLimiter); ok {
				a = backendUsageLimiterAdapter{id: b.ID, limiter: ul, now: s.now}
			}
		}
	}
	if a == nil {
		a = GenericAdapter{ID: b.ID, Now: s.now}
	}
	providerCtx, cancel := context.WithTimeout(ctx, ProviderTimeout)
	defer cancel()
	r := a.Fetch(providerCtx, b)
	r.BackendID = b.ID
	if r.ObservedAt.IsZero() {
		r.ObservedAt = now
	}
	if r.Usage == nil {
		r.Usage = []Limit{}
	}
	if !normalizeLimits(r.Usage) {
		r.Status = StatusError
		r.Usage = []Limit{}
		r.Error = &ProviderError{Code: "invalid_response", Message: "backend returned invalid usage-limit metadata"}
	}
	sort.SliceStable(r.Usage, func(i, j int) bool { return r.Usage[i].ID < r.Usage[j].ID })
	if r.Status == StatusOK || r.Status == StatusUnsupported {
		s.put(b, r, now)
		return project(b, r, false, false, nil)
	}
	if transient(r.Status) {
		if ce, ok := s.cached(b); ok && now.Sub(ce.storedAt) <= StaleTTL {
			return project(b, ce.result, true, true, r.Error)
		}
	}
	return project(b, r, false, false, nil)
}

func transient(s Status) bool {
	return s == StatusUnavailable || s == StatusTimeout || s == StatusError
}

func normalizeLimits(limits []Limit) bool {
	seen := make(map[string]struct{}, len(limits))
	for i := range limits {
		limit := &limits[i]
		if limit.ID == "" || limit.Scope == "" || limit.Label == "" {
			return false
		}
		if _, exists := seen[limit.ID]; exists {
			return false
		}
		seen[limit.ID] = struct{}{}
		if limit.UsedPercent != nil && (*limit.UsedPercent < 0 || *limit.UsedPercent > 100) {
			limit.UsedPercent = nil
			limit.RemainingPercent = nil
		}
		if limit.RemainingPercent != nil && (*limit.RemainingPercent < 0 || *limit.RemainingPercent > 100) {
			limit.RemainingPercent = nil
		}
		if limit.ModelFamilies != nil {
			sort.Strings(limit.ModelFamilies)
		}
		if limit.Models != nil {
			sort.Strings(limit.Models)
		}
	}
	return true
}
func (s *Service) cached(b backendstore.Backend) (cacheEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.cache[b.ID]
	if ok && (v.installed != b.Installed || v.binaryPath != b.BinaryPath) {
		delete(s.cache, b.ID)
		return cacheEntry{}, false
	}
	return v, ok
}
func (s *Service) put(b backendstore.Backend, r Result, now time.Time) {
	r = cloneResult(r)
	if r.Account != nil {
		r.Account.Label = ""
	}
	s.mu.Lock()
	s.cache[b.ID] = cacheEntry{result: r, storedAt: now, installed: b.Installed, binaryPath: b.BinaryPath}
	s.mu.Unlock()
}
func cloneResult(r Result) Result {
	r.Usage = append([]Limit(nil), r.Usage...)
	for i := range r.Usage {
		limit := &r.Usage[i]
		limit.ModelFamilies = append([]string(nil), limit.ModelFamilies...)
		limit.Models = append([]string(nil), limit.Models...)
		limit.UsedPercent = clonePtr(limit.UsedPercent)
		limit.RemainingPercent = clonePtr(limit.RemainingPercent)
		limit.DurationMinutes = clonePtr(limit.DurationMinutes)
		limit.ResetsAt = clonePtr(limit.ResetsAt)
		limit.LimitState = clonePtr(limit.LimitState)
	}
	if r.Account != nil {
		v := *r.Account
		r.Account = &v
	}
	if r.Error != nil {
		v := *r.Error
		r.Error = &v
	}
	return r
}

func clonePtr[T any](v *T) *T {
	if v == nil {
		return nil
	}
	clone := *v
	return &clone
}
func project(b backendstore.Backend, r Result, cached, stale bool, warning *ProviderError) BackendResult {
	r = cloneResult(r)
	if r.Account != nil {
		r.Account.Label = ""
	}
	if warning != nil {
		r.Error = &ProviderError{Code: warning.Code, Message: warning.Message}
	}
	return BackendResult{ID: b.ID, Tier: b.Tier, Installed: b.Installed, Enabled: b.Enabled, Status: r.Status, Account: r.Account, Usage: r.Usage, ObservedAt: r.ObservedAt, Cached: cached, Stale: stale, Error: r.Error}
}

type backendUsageLimiterAdapter struct {
	id      string
	limiter agentbackend.UsageLimiter
	now     func() time.Time
}

func (a backendUsageLimiterAdapter) BackendID() string { return a.id }

func (a backendUsageLimiterAdapter) Fetch(ctx context.Context, b backendstore.Backend) Result {
	now := clock(a.now)
	if !b.Installed {
		return notInstalled(b.ID, now)
	}
	res, ok := a.limiter.FetchUsage(ctx)
	if !ok {
		return unsupported(b.ID, "backend does not support live usage queries", now)
	}

	result := Result{
		BackendID:  b.ID,
		Status:     Status(res.Status),
		ObservedAt: res.ObservedAt,
	}
	if result.ObservedAt.IsZero() {
		result.ObservedAt = now
	}
	if res.Account != nil {
		result.Account = &Account{
			Plan:        res.Account.Plan,
			LoginMethod: res.Account.LoginMethod,
		}
	}
	if res.ErrorCode != "" || res.ErrorMsg != "" {
		result.Error = &ProviderError{
			Code:    res.ErrorCode,
			Message: res.ErrorMsg,
		}
	}
	result.Usage = make([]Limit, 0, len(res.Usage))
	for _, u := range res.Usage {
		result.Usage = append(result.Usage, Limit{
			ID:               u.ID,
			Scope:            u.Scope,
			Label:            u.Label,
			ModelFamilies:    u.ModelFamilies,
			Models:           u.Models,
			UsedPercent:      u.UsedPercent,
			RemainingPercent: u.RemainingPercent,
			DurationMinutes:  u.DurationMinutes,
			ResetsAt:         u.ResetsAt,
			LimitState:       u.LimitState,
		})
	}
	return result
}
