package ptyevent

import (
	"errors"
	"fmt"
	"time"
)

// ShapeStatus is a learned prompt shape's lifecycle stage.
type ShapeStatus string

const (
	// ShapeCandidate: proposed from a Fast-Brain reading. It may help the
	// classifier RECOGNIZE a prompt (provenance learned, not confident), but no
	// decision is ever taken on a candidate.
	ShapeCandidate ShapeStatus = "candidate"
	// ShapeValidated: re-observed consistently, verified on screen each time,
	// confirmed by a human answer or a parser, and backed by a regression
	// fixture. Still not usable for decisions until versioned.
	ShapeValidated ShapeStatus = "validated"
	// ShapeVersioned: pinned to a shape version, a backend CLI version, a
	// scope and an expiry. The only usable stage.
	ShapeVersioned ShapeStatus = "versioned"
	// ShapeRetired: struck (breaker trip, revalidation drift, human
	// contradiction) or superseded. Terminal; a re-learn starts a new
	// candidate with a new ID.
	ShapeRetired ShapeStatus = "retired"
)

// ShapeScopeKind is how widely a learned shape applies.
type ShapeScopeKind string

const (
	ShapeScopeSession ShapeScopeKind = "session"
	ShapeScopeProject ShapeScopeKind = "project"
	ShapeScopeUser    ShapeScopeKind = "user" // every project of this warden node
)

// LearnedShape is the stored record behind ProvLearned.
type LearnedShape struct {
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
	Backend     string `json:"backend"`
	// CLIVersion is the backend CLI version the shape was validated on. A
	// different version makes the shape unusable until re-validated (prompt
	// formats drift across releases).
	CLIVersion string           `json:"cli_version,omitempty"`
	ScopeKind  ShapeScopeKind   `json:"scope_kind,omitempty"`
	ScopeID    string           `json:"scope_id,omitempty"` // session or project id
	Status     ShapeStatus      `json:"status"`
	Version    int              `json:"version"`
	Class      PromptClass      `json:"class"`
	Caps       []Capability     `json:"capabilities,omitempty"`
	Semantics  []OptionSemantic `json:"semantics"` // per option, top-down

	Observations   int  `json:"observations"`    // verified, consistent sightings
	HumanConfirmed bool `json:"human_confirmed"` // a human answered it consistent with Semantics
	ParserAgreed   bool `json:"parser_agreed"`   // a deterministic parser produced the same reading
	// Fixtures are regression fixture ids (redacted screen + expected Prompt)
	// that must keep classifying identically; CI replays them.
	Fixtures []string `json:"fixtures"`

	CreatedAt     time.Time `json:"created_at"`
	ValidatedAt   time.Time `json:"validated_at,omitzero"`
	ExpiresAt     time.Time `json:"expires_at,omitzero"`
	RetiredAt     time.Time `json:"retired_at,omitzero"`
	RetiredReason string    `json:"retired_reason,omitempty"`
}

// ShapePolicy bounds promotion.
type ShapePolicy struct {
	MinObservations int
	MaxTTL          time.Duration
}

// DefaultShapePolicy is the contract default.
var DefaultShapePolicy = ShapePolicy{MinObservations: 3, MaxTTL: 30 * 24 * time.Hour}

// Promotion errors.
var (
	ErrShapeTransition = errors.New("illegal learned-shape transition")
	ErrShapeEvidence   = errors.New("insufficient evidence for promotion")
)

// Validate promotes candidate → validated.
func (s LearnedShape) Validate(now time.Time, pol ShapePolicy) (LearnedShape, error) {
	if s.Status != ShapeCandidate {
		return s, fmt.Errorf("%w: %s → validated", ErrShapeTransition, s.Status)
	}
	switch {
	case s.Observations < pol.MinObservations:
		return s, fmt.Errorf("%w: %d/%d consistent verified observations", ErrShapeEvidence, s.Observations, pol.MinObservations)
	case !s.HumanConfirmed && !s.ParserAgreed:
		return s, fmt.Errorf("%w: needs a human answer or parser agreement (never self-confirmed)", ErrShapeEvidence)
	case len(s.Fixtures) == 0:
		return s, fmt.Errorf("%w: no regression fixture", ErrShapeEvidence)
	case len(s.Semantics) == 0 || !s.Class.Valid() || s.Class == ClassUnknown:
		return s, fmt.Errorf("%w: shape has no classification", ErrShapeEvidence)
	case s.Class == ClassPermission && len(s.Caps) == 0:
		return s, fmt.Errorf("%w: permission shape names no capability", ErrShapeEvidence)
	}
	s.Status, s.ValidatedAt = ShapeValidated, now
	return s, nil
}

// Pin promotes validated → versioned (or renews an expired versioned
// shape, bumping Version) with an explicit CLI version, scope and TTL.
func (s LearnedShape) Pin(now time.Time, cliVersion string, kind ShapeScopeKind, scopeID string, ttl time.Duration, pol ShapePolicy) (LearnedShape, error) {
	renew := s.Status == ShapeVersioned && !now.Before(s.ExpiresAt)
	if s.Status != ShapeValidated && !renew {
		return s, fmt.Errorf("%w: %s → versioned", ErrShapeTransition, s.Status)
	}
	if cliVersion == "" {
		return s, fmt.Errorf("%w: a versioned shape must pin a CLI version", ErrShapeEvidence)
	}
	if kind == "" || (kind != ShapeScopeUser && scopeID == "") {
		return s, fmt.Errorf("%w: a versioned shape must be scoped", ErrShapeEvidence)
	}
	if ttl <= 0 || ttl > pol.MaxTTL {
		ttl = pol.MaxTTL
	}
	if len(s.Fixtures) == 0 {
		return s, fmt.Errorf("%w: no regression fixture", ErrShapeEvidence)
	}
	s.Status, s.Version = ShapeVersioned, s.Version+1
	s.CLIVersion, s.ScopeKind, s.ScopeID = cliVersion, kind, scopeID
	s.ExpiresAt = now.Add(ttl)
	return s, nil
}

// Retire strikes a shape. Retiring is always legal and irreversible.
func (s LearnedShape) Retire(now time.Time, reason string) LearnedShape {
	s.Status, s.RetiredAt, s.RetiredReason = ShapeRetired, now, reason
	return s
}

// ShapeQuery is the context a match happens in.
type ShapeQuery struct {
	Now        time.Time
	Backend    string
	CLIVersion string
	SessionID  string
	ProjectID  string
}

// Usable reports whether a decision may rely on this shape here and now:
// versioned, unexpired, same backend AND CLI version, in scope. Anything else
// — including a candidate or validated shape — can at most help recognition;
// Confident() rejects the prompt and the state machine routes it to
// needs_attention. Learned shapes never approve anything by themselves: they
// only classify, and Decide still applies the posture.
func (s LearnedShape) Usable(q ShapeQuery) bool {
	if s.Status != ShapeVersioned || !q.Now.Before(s.ExpiresAt) {
		return false
	}
	if s.Backend != q.Backend || s.CLIVersion != q.CLIVersion {
		return false
	}
	switch s.ScopeKind {
	case ShapeScopeUser:
		return true
	case ShapeScopeProject:
		return s.ScopeID != "" && s.ScopeID == q.ProjectID
	case ShapeScopeSession:
		return s.ScopeID != "" && s.ScopeID == q.SessionID
	}
	return false
}
