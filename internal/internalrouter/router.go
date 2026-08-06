// Package internalrouter routes warden's own internal thinking — task
// classification, activity summaries, agent naming, digest narration, and memory
// curation — STRICTLY through free and local backends (docs/specs/
// 2026-08-06-backend-registry.md §7). It NEVER makes a paid call: the walk it
// produces is the ordered attempt list from the backend registry's
// internal-thinking mode, and when that list is exhausted the caller degrades
// gracefully (deterministic slug / skipped narration / default bucket / no
// curate proposal) rather than escalating to a subscription/pay-per-use backend.
//
// The Router holds the backendstore (source of truth for the mode plus each
// backend's tier / installed / enabled / limited state) and the optional local
// llm.Completer (the terminal, never-limited candidate). It runs a free CLI
// candidate via its HeadlessCmd; on a rate-limit / spend signal it stamps that
// backend's LimitedUntil (config backends.limit_retry, default 15m) and continues
// down the list.
package internalrouter

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/llm"
)

// ErrNoCandidate is returned by Complete when the candidate walk is exhausted —
// no free CLI backend served and the local model is absent or errored. The
// caller treats it as the signal to degrade gracefully (§7.2), NEVER to make a
// paid call.
var ErrNoCandidate = errors.New("internalrouter: no free or local candidate available")

// defaultCallTimeout bounds one free-CLI headless invocation, mirroring
// lifecycle's claudeCallTimeout so a stuck CLI cannot block the walk.
const defaultCallTimeout = 30 * time.Second

// Candidate is one attempt in the internal-thinking walk: a free CLI backend
// (IsLocal=false, ID its registry id) or the reserved local model (IsLocal=true,
// ID backendstore.IDLocal), which is always last and terminates the walk.
type Candidate struct {
	ID      string
	IsLocal bool
}

// Runner executes a resolved argv. It is the same shape as lifecycle.Runner, so
// the daemon passes its existing runner straight through.
type Runner interface {
	Run(ctx context.Context, dir, name string, args ...string) (string, error)
}

// storeAPI is the slice of the backendstore the router reads and stamps. The
// concrete *backendstore.Store satisfies it; tests can substitute a fake.
type storeAPI interface {
	List() ([]backendstore.Backend, error)
	Settings() (backendstore.Settings, error)
	SetLimited(id string, until time.Time) error
}

// Router walks the free/local candidate list for one internal-thinking prompt.
type Router struct {
	store storeAPI
	local llm.Completer // the local model (terminal candidate); nil when local_llm is off
	// headless runs a free-CLI candidate's HeadlessCmd and returns its output.
	// A seam so tests exercise the walk without a real backend binary; New wires
	// the production implementation over agentbackend + the Runner.
	headless    func(ctx context.Context, backendID, prompt string) (string, error)
	limitTTL    time.Duration
	callTimeout time.Duration
	now         func() time.Time
}

// New builds a Router over the backend registry store, the optional local
// completer (nil ⇒ local_llm off), the Runner used to execute a free CLI
// backend's HeadlessCmd, and the limit-retry TTL (config backends.limit_retry).
func New(store *backendstore.Store, local llm.Completer, run Runner, limitTTL time.Duration) *Router {
	r := &Router{
		store:       store,
		local:       local,
		limitTTL:    limitTTL,
		callTimeout: defaultCallTimeout,
		now:         time.Now,
	}
	r.headless = func(ctx context.Context, id, prompt string) (string, error) {
		return runHeadless(ctx, run, r.callTimeout, id, prompt)
	}
	return r
}

// runHeadless resolves a backend by id and runs its headless one-shot with a
// bounded timeout. A backend with no headless mode is an error the walk skips.
func runHeadless(ctx context.Context, run Runner, timeout time.Duration, id, prompt string) (string, error) {
	b, err := agentbackend.Get(id)
	if err != nil {
		return "", err
	}
	argv, ok := b.HeadlessCmd(prompt)
	if !ok || len(argv) == 0 {
		return "", fmt.Errorf("backend %s has no headless mode", id)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return run.Run(cctx, "", argv[0], argv[1:]...)
}

// Candidates returns the ordered attempt list for the store's current
// internal-thinking mode (§7):
//   - local_only      -> [local]
//   - free_plus_local -> [eligible free CLI backends (default-first, then stable
//     id), …, local]
//
// A free CLI backend is eligible ONLY when Installed && Enabled && Tier=="free"
// && its LimitedUntil is in the past. The local row is always last (and, in
// local_only, the only candidate). A paid (subscription / pay_per_use),
// unclassified, or disabled backend is NEVER included.
func (r *Router) Candidates() ([]Candidate, error) {
	settings, err := r.store.Settings()
	if err != nil {
		return nil, err
	}
	local := Candidate{ID: backendstore.IDLocal, IsLocal: true}
	if settings.InternalThinkingMode == backendstore.ThinkingModeLocalOnly {
		return []Candidate{local}, nil
	}
	// free_plus_local (the default): eligible free CLIs first, local last.
	rows, err := r.store.List()
	if err != nil {
		return nil, err
	}
	now := r.now()
	free := make([]backendstore.Backend, 0, len(rows))
	for _, b := range rows {
		if eligibleFree(b, now) {
			free = append(free, b)
		}
	}
	// Default backend first, then stable id order (deterministic across restarts).
	sort.SliceStable(free, func(i, j int) bool {
		if free[i].Default != free[j].Default {
			return free[i].Default
		}
		return free[i].ID < free[j].ID
	})
	out := make([]Candidate, 0, len(free)+1)
	for _, b := range free {
		out = append(out, Candidate{ID: b.ID})
	}
	return append(out, local), nil
}

// eligibleFree reports whether b is a free CLI backend the router may call right
// now: installed, enabled, tier free, and not currently limited. The reserved
// local row is excluded here (it is appended as the terminal candidate).
func eligibleFree(b backendstore.Backend, now time.Time) bool {
	if b.IsLocal {
		return false
	}
	if b.Tier != backendstore.TierFree {
		return false
	}
	if !b.Installed || !b.Enabled {
		return false
	}
	return b.LimitedUntil.Before(now)
}

// Complete walks the candidate list for prompt and returns the first candidate's
// completion. For a free CLI candidate it runs the backend's HeadlessCmd; a
// rate-limit / spend signal stamps that backend's LimitedUntil (now+limitTTL) and
// continues, and any other error simply moves to the next candidate. The local
// candidate calls the completer and terminates the walk (success or error). When
// the list is exhausted it returns ErrNoCandidate so the caller degrades
// gracefully — Complete never makes a paid call. It implements llm.Completer.
func (r *Router) Complete(ctx context.Context, prompt string) (string, error) {
	cands, err := r.Candidates()
	if err != nil {
		return "", err
	}
	for _, c := range cands {
		if c.IsLocal {
			if r.local == nil {
				continue // local_llm off — nothing terminal to fall back to
			}
			return r.local.Complete(ctx, prompt) // terminal: never limited
		}
		out, runErr := r.headless(ctx, c.ID, prompt)
		if runErr != nil {
			if isLimitSignal(out) || isLimitSignal(runErr.Error()) {
				r.stampLimited(c.ID)
			}
			continue // non-limit errors also fall through to the next candidate
		}
		if isLimitSignal(out) {
			r.stampLimited(c.ID)
			continue
		}
		return out, nil
	}
	return "", ErrNoCandidate
}

// stampLimited records a limit hit on backend id (best-effort; a store error only
// means the backend is retried sooner, never a paid call).
func (r *Router) stampLimited(id string) {
	_ = r.store.SetLimited(id, r.now().Add(r.limitTTL))
}

// limitSignalRe anchors on the distinctive rate-limit / spend-cap phrases a CLI
// prints when it refuses a call, so ordinary agent prose does not trip it. It is
// deliberately broader than the poller's live-pane banner matcher (poller/
// detect.go) because a headless one-shot's stderr/stdout carries no menu and
// varies by backend.
var limitSignalRe = regexp.MustCompile(
	`(?i)(rate[\s-]?limit|usage limit|session limit|weekly limit|quota exceeded|spend limit|too many requests|429|limit reached|overloaded)`,
)

// isLimitSignal reports whether s looks like a backend's rate-limit / spend
// refusal — the trigger to stamp LimitedUntil and skip to the next candidate.
func isLimitSignal(s string) bool {
	return limitSignalRe.MatchString(s)
}
