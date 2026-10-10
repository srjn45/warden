package fastbrain

import (
	"context"
	"errors"
	"time"
)

// ErrNoCandidate means no configured runner candidate was eligible and healthy.
// The engine maps it to StatusNoRunner so callers fail open.
var ErrNoCandidate = errors.New("fastbrain: no eligible runner candidate")

// Candidate is one provider/AI-CLI/model option for a tier. ID, Provider and
// Model are bounded labels (never agent ids, prompts or paths). The source
// orders candidates by policy preference; the pool never reorders them.
type Candidate struct {
	ID       string // bounded label, e.g. the backend id
	Provider string
	Model    string // "" = backend default
	CostTier string // free | subscription | pay_per_use | local
	Runner   Runner
	// Ineligible, when non-empty, is a bounded reason code (disabled,
	// not_installed, no_headless, rate_limited, paid_tier, ...). The candidate
	// is recorded as rejected and never run.
	Ineligible string
}

// CandidateSource lists the candidates for a tier in preference order.
type CandidateSource func(tier Tier) []Candidate

// CandidateVerdict is why a candidate was or was not used in a selection.
type CandidateVerdict string

const (
	VerdictSelected CandidateVerdict = "selected" // ran and produced the result
	VerdictRejected CandidateVerdict = "rejected" // not run; Reason says why
	VerdictFailed   CandidateVerdict = "failed"   // ran and failed; fell back
)

// CandidateOutcome is one trail entry of a selection.
type CandidateOutcome struct {
	ID      string
	Model   string
	Verdict CandidateVerdict
	Reason  string
}

// Selection is the redacted record of how a runner was chosen for one call.
type Selection struct {
	Runner   string // winning candidate id; "" when none succeeded
	Model    string
	Attempts int
	Fallback bool // true when a candidate other than the first attempted won
	Trail    []CandidateOutcome
}

const maxTrail = 12

// DetailedRunner is a Runner that also reports its selection trail.
type DetailedRunner interface {
	Runner
	RunDetailed(ctx context.Context, prompt string) (string, Selection, error)
}

// PoolOptions tunes candidate-pool behaviour.
type PoolOptions struct {
	// FirstAttemptShare is the fraction of the remaining deadline a first
	// attempt may use when another healthy candidate could still take over.
	// Zero means 0.6.
	FirstAttemptShare float64
}

// Pool is a tier Runner that selects among policy-ordered candidates using
// eligibility and circuit health, tries each at most once per call, and fails
// open (ErrNoCandidate) rather than defaulting to any particular provider.
type Pool struct {
	tier   Tier
	source CandidateSource
	health *Health
	opts   PoolOptions
}

// NewPool builds a pool for tier. health may be shared across tiers so one
// provider outage opens one circuit.
func NewPool(tier Tier, source CandidateSource, health *Health, opts PoolOptions) *Pool {
	if health == nil {
		health = NewHealth(HealthOptions{})
	}
	if opts.FirstAttemptShare <= 0 || opts.FirstAttemptShare >= 1 {
		opts.FirstAttemptShare = 0.6
	}
	return &Pool{tier: tier, source: source, health: health, opts: opts}
}

// Health exposes the shared tracker for metrics and tests.
func (p *Pool) Health() *Health { return p.health }

// Run implements Runner.
func (p *Pool) Run(ctx context.Context, prompt string) (string, error) {
	out, _, err := p.RunDetailed(ctx, prompt)
	return out, err
}

// RunDetailed implements DetailedRunner.
func (p *Pool) RunDetailed(ctx context.Context, prompt string) (string, Selection, error) {
	var sel Selection
	add := func(c Candidate, v CandidateVerdict, reason string) {
		if len(sel.Trail) < maxTrail {
			sel.Trail = append(sel.Trail, CandidateOutcome{ID: c.ID, Model: c.Model, Verdict: v, Reason: reason})
		}
	}
	cands := p.source(p.tier)
	var lastErr error
	firstTried := ""
	for i, c := range cands {
		if c.Ineligible != "" || c.Runner == nil {
			reason := c.Ineligible
			if reason == "" {
				reason = "no_runner"
			}
			add(c, VerdictRejected, reason)
			continue
		}
		if ctx.Err() != nil {
			break
		}
		if !p.health.Allow(c.ID) {
			add(c, VerdictRejected, "circuit_open")
			continue
		}
		actx, cancel := ctx, context.CancelFunc(func() {})
		if p.moreAfter(cands, i) {
			if dl, ok := ctx.Deadline(); ok {
				if rem := time.Until(dl); rem > 0 {
					actx, cancel = context.WithTimeout(ctx, time.Duration(float64(rem)*p.opts.FirstAttemptShare))
				}
			}
		}
		sel.Attempts++
		if firstTried == "" {
			firstTried = c.ID
		}
		out, err := c.Runner.Run(actx, prompt)
		cancel()
		switch {
		case err == nil:
			p.health.Success(c.ID)
			add(c, VerdictSelected, "ok")
			sel.Runner, sel.Model, sel.Fallback = c.ID, c.Model, c.ID != firstTried
			return out, sel, nil
		case errors.Is(ctx.Err(), context.Canceled):
			// The caller went away; this says nothing about the runner.
			p.health.Release(c.ID)
			add(c, VerdictFailed, "canceled")
			return "", sel, err
		case ctx.Err() != nil:
			// Whole budget spent in this attempt: a timeout, no time to fall back.
			p.health.Failure(c.ID, FailTimeout)
			add(c, VerdictFailed, string(FailTimeout))
			return "", sel, err
		}
		class := FailRunnerError
		if errors.Is(err, context.DeadlineExceeded) {
			class = FailTimeout
		}
		p.health.Failure(c.ID, class)
		add(c, VerdictFailed, string(class))
		lastErr = err
	}
	if sel.Attempts == 0 {
		if ctx.Err() != nil {
			return "", sel, ctx.Err()
		}
		return "", sel, ErrNoCandidate
	}
	if lastErr == nil {
		lastErr = ctx.Err()
	}
	return "", sel, lastErr
}

// moreAfter reports whether a later candidate could still take over.
func (p *Pool) moreAfter(cands []Candidate, i int) bool {
	for _, c := range cands[i+1:] {
		if c.Ineligible == "" && c.Runner != nil && p.health.Peek(c.ID) {
			return true
		}
	}
	return false
}
