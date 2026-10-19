package decisionrouter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/srjn45/warden/internal/ptyevent"
)

// ShapeFixture is a deterministic regression fixture for a learned shape: the
// redacted screen, the frozen Fast-Brain reading it produced, and the
// classification that reading must keep yielding. Replaying it needs no model
// and no network, so CI (and Promote) can prove a shape still classifies
// identically after warden or the adapter changes.
type ShapeFixture struct {
	ID          string                `json:"id"`
	Backend     string                `json:"backend"`
	CLIVersion  string                `json:"cli_version,omitempty"`
	Screen      string                `json:"screen"` // redacted
	Recognition fastbrain.Recognition `json:"recognition"`
	Expect      FixtureExpect         `json:"expect"`
}

// FixtureExpect is the classification a fixture pins.
type FixtureExpect struct {
	Fingerprint string                    `json:"fingerprint"`
	Class       ptyevent.PromptClass      `json:"class"`
	Caps        []ptyevent.Capability     `json:"capabilities"`
	Semantics   []ptyevent.OptionSemantic `json:"semantics"`
	Default     int                       `json:"default"`
}

// FixtureStore persists regression fixtures.
type FixtureStore interface {
	Save(ShapeFixture) error
	Load(id string) (ShapeFixture, error)
	List() ([]ShapeFixture, error)
}

// DirFixtureStore keeps one JSON file per fixture, in the same style as the
// contract's testdata/prompts corpus, so a fixture can be reviewed and promoted
// into the repository as-is.
type DirFixtureStore struct{ Dir string }

// Save writes the fixture atomically.
func (d DirFixtureStore) Save(fx ShapeFixture) error {
	if fx.ID == "" {
		return errors.New("fixture has no id")
	}
	if err := os.MkdirAll(d.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(fx, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(d.Dir, fx.ID+".json"), append(b, '\n'))
}

// Load reads one fixture.
func (d DirFixtureStore) Load(id string) (ShapeFixture, error) {
	if strings.ContainsAny(id, `/\`) || id == "" {
		return ShapeFixture{}, fmt.Errorf("invalid fixture id %q", id)
	}
	b, err := os.ReadFile(filepath.Join(d.Dir, id+".json"))
	if err != nil {
		return ShapeFixture{}, err
	}
	var fx ShapeFixture
	return fx, json.Unmarshal(b, &fx)
}

// List returns every fixture, ordered by id.
func (d DirFixtureStore) List() ([]ShapeFixture, error) {
	ents, err := os.ReadDir(d.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []ShapeFixture
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		fx, err := d.Load(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, fx)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// frozenRecognizer replays one recorded reading.
type frozenRecognizer struct{ rec fastbrain.Recognition }

func (f frozenRecognizer) Recognize(context.Context, string, string) (fastbrain.Recognition, bool, error) {
	return f.rec, true, nil
}

// Replay re-classifies the fixture's screen from its frozen reading and checks
// the result still equals what the fixture pins. It is fully deterministic.
func (fx ShapeFixture) Replay(ctx context.Context, now time.Time) error {
	c := NewPromptClassifierWith(nil, frozenRecognizer{rec: fx.Recognition})
	res, err := c.Classify(ctx, ClassifyRequest{
		Screen:     fx.Screen,
		BackendID:  fx.Backend,
		CLIVersion: fx.CLIVersion,
		Now:        now,
	})
	if err != nil {
		return fmt.Errorf("fixture %s: classify: %w", fx.ID, err)
	}
	p := res.Prompt
	if p == nil {
		return fmt.Errorf("fixture %s: no prompt", fx.ID)
	}
	got := expectOf(p)
	if !reflect.DeepEqual(got, fx.Expect) {
		return fmt.Errorf("fixture %s: classification drifted\n got: %+v\nwant: %+v", fx.ID, got, fx.Expect)
	}
	return nil
}

func expectOf(p *ptyevent.Prompt) FixtureExpect {
	e := FixtureExpect{Fingerprint: p.Fingerprint, Class: p.Class, Caps: p.Capabilities}
	for _, o := range p.Options {
		e.Semantics = append(e.Semantics, o.Semantic)
		if o.Default {
			e.Default = o.Index
		}
	}
	return e
}

// Learner turns Fast-Brain readings of unfamiliar prompts into scoped,
// versioned, expiring learned shapes. It only ever moves a shape forward on
// evidence the contract names (§9.6): repeated verified sightings, a human
// answer or deterministic-parser agreement (never self-confirmation), and a
// regression fixture that replays. A learned shape classifies; Decide still
// applies the posture, so nothing here approves anything.
type Learner struct {
	Shapes   LearnedShapeStore
	Fixtures FixtureStore
	Policy   ptyevent.ShapePolicy
	Clock    func() time.Time
}

func (l *Learner) now() time.Time {
	if l.Clock != nil {
		return l.Clock()
	}
	return time.Now()
}

func (l *Learner) policy() ptyevent.ShapePolicy {
	if l.Policy.MinObservations <= 0 {
		return ptyevent.DefaultShapePolicy
	}
	return l.Policy
}

// Observe records the regression fixture behind a candidate produced by
// Classify. It is a no-op for readings that did not yield a candidate (low
// confidence, unverified on screen, unknown class). The screen is redacted
// before it is written, and so are the model's transcribed strings.
func (l *Learner) Observe(req ClassifyRequest, res ClassifyResult) error {
	if res.CandidateShape == nil || res.Recognition == nil || res.Prompt == nil {
		return nil
	}
	fx := ShapeFixture{
		Backend:     res.Prompt.Backend,
		CLIVersion:  req.CLIVersion,
		Screen:      redactString(res.RawScreen),
		Recognition: redactRecognition(*res.Recognition),
		Expect:      expectOf(res.Prompt),
	}
	// The fixture's expectation is derived from a replay of its own redacted
	// form, so redaction can never make a stored fixture unreplayable.
	replayed, err := l.replayPrompt(fx)
	if err != nil {
		return err
	}
	fx.Expect = expectOf(replayed)
	fx.ID = fixtureID(fx)
	if l.Fixtures != nil {
		if err := l.Fixtures.Save(fx); err != nil {
			return err
		}
	}
	if l.Shapes == nil {
		return nil
	}
	if _, ok := l.Shapes.GetByFingerprint(res.CandidateShape.Fingerprint); !ok {
		if _, err := l.Shapes.AddCandidate(*res.CandidateShape); err != nil {
			return err
		}
	}
	_, err = l.Shapes.AttachFixture(res.CandidateShape.Fingerprint, fx.ID)
	return err
}

func (l *Learner) replayPrompt(fx ShapeFixture) (*ptyevent.Prompt, error) {
	c := NewPromptClassifierWith(nil, frozenRecognizer{rec: fx.Recognition})
	res, err := c.Classify(context.Background(), ClassifyRequest{
		Screen: fx.Screen, BackendID: fx.Backend, CLIVersion: fx.CLIVersion, Now: l.now(),
	})
	if err != nil {
		return nil, err
	}
	if res.Prompt == nil || res.Prompt.Provenance != ptyevent.ProvFastBrain {
		return nil, fmt.Errorf("reading of fixture does not replay")
	}
	return res.Prompt, nil
}

func fixtureID(fx ShapeFixture) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s", fx.Backend, fx.CLIVersion, fx.Screen)
	return "fx-" + hex.EncodeToString(h.Sum(nil))[:16]
}

func redactRecognition(r fastbrain.Recognition) fastbrain.Recognition {
	r.Question = redactString(r.Question)
	r.Action = redactString(r.Action)
	r.Rationale = redactString(r.Rationale)
	opts := make([]string, len(r.Options))
	for i, o := range r.Options {
		opts[i] = redactString(o)
	}
	r.Options = opts
	return r
}

// ParserAgreement records that a deterministic parser produced a prompt with
// the same fingerprint and option semantics as a candidate: one of the two
// independent confirmations a shape needs.
func (l *Learner) ParserAgreement(p *ptyevent.Prompt) {
	if l.Shapes == nil || p == nil || p.Provenance != ptyevent.ProvParser {
		return
	}
	sh, ok := l.Shapes.GetByFingerprint(p.Fingerprint)
	if !ok || sh.Status != ptyevent.ShapeCandidate || !sameSemantics(sh.Semantics, p.Options) {
		return
	}
	_, _ = l.Shapes.RecordObservation(p.Fingerprint, "", false, true)
}

// HumanAnswered records a human's choice on a prompt. If the option they chose
// carries the semantic the candidate predicted, that confirms it; a human
// choosing otherwise is a contradiction and retires the shape.
func (l *Learner) HumanAnswered(p *ptyevent.Prompt, chosen int) {
	if l.Shapes == nil || p == nil || chosen < 1 || chosen > len(p.Options) {
		return
	}
	sh, ok := l.Shapes.GetByFingerprint(p.Fingerprint)
	if !ok || sh.Status == ptyevent.ShapeRetired || chosen > len(sh.Semantics) {
		return
	}
	if sh.Semantics[chosen-1] == p.Options[chosen-1].Semantic {
		_, _ = l.Shapes.RecordObservation(p.Fingerprint, "", true, false)
		return
	}
	_, _ = l.Shapes.Retire(sh.ID, l.now(), "human choice contradicted the learned semantics")
}

func sameSemantics(sems []ptyevent.OptionSemantic, opts []ptyevent.Option) bool {
	if len(sems) != len(opts) {
		return false
	}
	for i := range sems {
		if sems[i] != opts[i].Semantic {
			return false
		}
	}
	return true
}

// Promote advances a candidate to a versioned, scoped, expiring shape: every
// stored regression fixture must replay first, then the contract's evidence
// rules (Validate) and the pin (Pin) apply. It returns the usable shape.
func (l *Learner) Promote(ctx context.Context, fp, cliVersion string, kind ptyevent.ShapeScopeKind, scopeID string, ttl time.Duration) (ptyevent.LearnedShape, error) {
	if l.Shapes == nil {
		return ptyevent.LearnedShape{}, errors.New("no shape store")
	}
	sh, ok := l.Shapes.GetByFingerprint(fp)
	if !ok {
		return ptyevent.LearnedShape{}, fmt.Errorf("no shape for fingerprint %s", fp)
	}
	now := l.now()
	if l.Fixtures != nil {
		for _, id := range sh.Fixtures {
			fx, err := l.Fixtures.Load(id)
			if err != nil {
				return sh, fmt.Errorf("fixture %s: %w", id, err)
			}
			if err := fx.Replay(ctx, now); err != nil {
				return sh, err
			}
		}
	}
	if sh.Status == ptyevent.ShapeCandidate {
		var err error
		if sh, err = l.Shapes.Validate(sh.ID, now, l.policy()); err != nil {
			return sh, err
		}
	}
	return l.Shapes.Pin(sh.ID, now, cliVersion, kind, scopeID, ttl, l.policy())
}

// RegressionCheck replays every stored fixture and reports the first drift.
func (l *Learner) RegressionCheck(ctx context.Context) error {
	if l.Fixtures == nil {
		return nil
	}
	all, err := l.Fixtures.List()
	if err != nil {
		return err
	}
	now := l.now()
	for _, fx := range all {
		if err := fx.Replay(ctx, now); err != nil {
			return err
		}
	}
	return nil
}
