package decisionrouter

import (
	"context"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/srjn45/warden/internal/ptyevent"
)

// PromptClassifier combines deterministic backend parsers, versioned learned shapes,
// and Fast-Brain prompt recognition into a normalized Prompt.
type PromptClassifier struct {
	shapeStore LearnedShapeStore
	recognizer Recognizer
}

// Recognizer reads a (redacted) screen the deterministic parsers could not. The
// production implementation asks Fast-Brain; fixtures replay a frozen reading so
// regression tests are deterministic and never touch a hosted provider.
type Recognizer interface {
	Recognize(ctx context.Context, redactedScreen, agentID string) (fastbrain.Recognition, bool, error)
}

type fastbrainRecognizer struct {
	e interface {
		Decide(ctx context.Context, req fastbrain.Request) (fastbrain.Response, error)
	}
}

func (f fastbrainRecognizer) Recognize(ctx context.Context, screen, agentID string) (fastbrain.Recognition, bool, error) {
	return fastbrain.RecognizePromptFor(ctx, f.e, screen, agentID)
}

// NewPromptClassifier creates a new PromptClassifier.
func NewPromptClassifier(store LearnedShapeStore, fb interface {
	Decide(ctx context.Context, req fastbrain.Request) (fastbrain.Response, error)
}) *PromptClassifier {
	c := &PromptClassifier{shapeStore: store}
	if fb != nil {
		c.recognizer = fastbrainRecognizer{e: fb}
	}
	return c
}

// NewPromptClassifierWith is NewPromptClassifier with an explicit Recognizer.
func NewPromptClassifierWith(store LearnedShapeStore, r Recognizer) *PromptClassifier {
	return &PromptClassifier{shapeStore: store, recognizer: r}
}

// Classify produces a bounded classification: permission, strategic decision,
// informational/idle, or unknown.
func (c *PromptClassifier) Classify(ctx context.Context, req ClassifyRequest) (ClassifyResult, error) {
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}

	screen := req.Screen
	if strings.TrimSpace(screen) == "" {
		return ClassifyResult{IsInformational: true}, nil
	}

	backendID := req.BackendID
	if backendID == "" && req.Backend != nil {
		backendID = req.Backend.ID()
	}

	// 1. Detect if screen is informational/idle or working
	if req.Backend != nil {
		st := req.Backend.DetectState(screen)
		if st == agentbackend.StateWorking || st == agentbackend.StateIdle {
			return ClassifyResult{IsInformational: true, RawScreen: screen}, nil
		}
	}

	// 2. Try deterministic backend parser
	if req.Backend != nil {
		app, ok := req.Backend.ParseApproval(screen)
		if ok && app != nil {
			prompt, err := c.approvalToPrompt(req.Backend, app, backendID, req.CLIVersion, screen, req.Workdir, req.TargetInWorkspace)
			if err == nil && prompt != nil && prompt.Validate() == nil {
				return ClassifyResult{
					Prompt:    prompt,
					RawScreen: screen,
				}, nil
			}
		}
	}

	// 3. Fallback / Provider CLI format adaptation: Fast-Brain prompt recognition + Learned Shape check
	if c.recognizer != nil && !req.Deterministic {
		// The screen reaches a model only after redaction.
		redacted, _ := ptyevent.Redact([]byte(screen))
		rec, recognized, err := c.recognizer.Recognize(ctx, string(redacted), req.AgentID)
		if err == nil && recognized && len(rec.Options) > 0 {
			// Locate and verify all options against the live screen
			loc, verified := agentbackend.LocateOptions(screen, rec.Options)

			fp := ptyevent.Fingerprint(backendID, 1, rec.Question, rec.Options)

			// Check if a usable versioned learned shape exists for this fingerprint
			if c.shapeStore != nil && verified {
				q := ptyevent.ShapeQuery{
					Now:        now,
					Backend:    backendID,
					CLIVersion: req.CLIVersion,
					SessionID:  req.SessionID,
					ProjectID:  req.ProjectID,
				}
				if sh, ok := c.shapeStore.GetByFingerprint(fp); ok && sh.Usable(q) {
					prompt := c.learnedShapeToPrompt(sh, rec, loc, backendID, screen, req.Workdir)
					if prompt != nil && prompt.Validate() == nil {
						redactEvidence(prompt, screen)
						return ClassifyResult{
							Prompt:    prompt,
							RawScreen: screen,
						}, nil
					}
				}
			}

			// Otherwise, build Fast-Brain prompt reading
			prompt, candShape, err := c.fastbrainToPrompt(rec, loc, verified, backendID, req.CLIVersion, screen, req.Workdir, req.ProjectID, now)
			if err == nil && prompt != nil {
				redactEvidence(prompt, screen)
				// Each verified, confident sighting counts once toward the
				// candidate; the Learner attaches its regression fixture.
				if candShape != nil && c.shapeStore != nil {
					_, _ = c.shapeStore.AddCandidate(*candShape)
				}
				return ClassifyResult{
					Prompt:         prompt,
					CandidateShape: candShape,
					Recognition:    &rec,
					RawScreen:      screen,
				}, nil
			}
		}
	}

	// 4. Unknown / Ambiguous screen
	fp := ptyevent.Fingerprint(backendID, 1, "unknown", nil)
	ik := ptyevent.InstanceKey(fp, "unknown", "unknown")
	unknownPrompt := &ptyevent.Prompt{
		Fingerprint:    fp,
		ShapeVersion:   1,
		InstanceKey:    ik,
		Backend:        backendID,
		BackendVersion: req.CLIVersion,
		Class:          ptyevent.ClassUnknown,
		Question:       "Unrecognized prompt or screen",
		Options: []ptyevent.Option{
			{Index: 1, Label: "Unknown", Semantic: ptyevent.SemCancel, Scope: ""},
		},
		CancelPath: ptyevent.CancelPath{Key: "Escape", OptionIndex: 1},
		Provenance: ptyevent.ProvParser,
		Confidence: 0.0,
		Verified:   false,
		Evidence: ptyevent.PromptEvidence{
			Raw: screen,
		},
	}

	return ClassifyResult{
		Prompt:    unknownPrompt,
		RawScreen: screen,
	}, nil
}

func (c *PromptClassifier) approvalToPrompt(b agentbackend.Backend, app *agentbackend.Approval, backendID, cliVersion, screen, workdir string, explicitTargetInWorkspace bool) (*ptyevent.Prompt, error) {
	return NormalizeApproval(b, app, NormalizeContext{
		BackendID:         backendID,
		CLIVersion:        cliVersion,
		Screen:            screen,
		Workdir:           workdir,
		TargetInWorkspace: explicitTargetInWorkspace,
	})
}

func (c *PromptClassifier) learnedShapeToPrompt(
	shape ptyevent.LearnedShape,
	rec fastbrain.Recognition,
	loc agentbackend.MenuLocation,
	backendID, screen, workdir string,
) *ptyevent.Prompt {
	fp := shape.Fingerprint
	subject := "CLI"
	target := rec.Action
	if rec.Trust {
		subject = "workspace"
		if target == "" {
			target = "trust_directory"
		}
	} else if rec.Action != "" {
		if idx := strings.Index(rec.Action, "("); idx > 0 && strings.HasSuffix(rec.Action, ")") {
			subject = strings.TrimSpace(rec.Action[:idx])
			target = strings.TrimSpace(rec.Action[idx+1 : len(rec.Action)-1])
		}
	}

	ik := ptyevent.InstanceKey(fp, subject, target)

	options := make([]ptyevent.Option, len(rec.Options))
	denyIdx := 0
	for i, lbl := range rec.Options {
		idx := i + 1
		sem := shape.Semantics[i]
		if sem.Refuses() && denyIdx == 0 {
			denyIdx = idx
		}
		scope := ptyevent.ScopeOnce
		if sem == ptyevent.SemAllowAlways {
			if rec.Trust {
				scope = ptyevent.ScopeWorkspace
			} else {
				scope = ptyevent.ScopeProject
			}
		} else if !sem.Allows() {
			scope = ""
		}
		options[i] = ptyevent.Option{
			Index:    idx,
			Label:    lbl,
			Semantic: sem,
			Scope:    scope,
			Default:  idx == loc.Selected,
		}
	}

	cancel := ptyevent.CancelPath{Key: "Escape"}
	if denyIdx > 0 {
		cancel.OptionIndex = denyIdx
	}

	p := &ptyevent.Prompt{
		Fingerprint:       fp,
		ShapeVersion:      shape.Version,
		InstanceKey:       ik,
		Backend:           backendID,
		BackendVersion:    shape.CLIVersion,
		Class:             shape.Class,
		Capabilities:      shape.Caps,
		Subject:           subject,
		Target:            target,
		TargetInWorkspace: IsTargetInWorkspace(target, workdir),
		Question:          rec.Question,
		Options:           options,
		CancelPath:        cancel,
		Navigate:          loc.Selected > 0,
		Provenance:        ptyevent.ProvLearned,
		Confidence:        ptyevent.MinConfidenceLearned,
		Verified:          true,
		LearnedShapeID:    shape.ID,
		Evidence: ptyevent.PromptEvidence{
			Raw: screen,
		},
	}

	return p
}

func (c *PromptClassifier) fastbrainToPrompt(
	rec fastbrain.Recognition,
	loc agentbackend.MenuLocation,
	verified bool,
	backendID, cliVersion, screen, workdir, projectID string,
	now time.Time,
) (*ptyevent.Prompt, *ptyevent.LearnedShape, error) {
	class := ptyevent.ClassPermission
	subject := "CLI"
	target := rec.Action

	if rec.Trust {
		subject = "workspace"
		if target == "" {
			target = "trust_directory"
		}
	} else if rec.Action != "" {
		if idx := strings.Index(rec.Action, "("); idx > 0 && strings.HasSuffix(rec.Action, ")") {
			subject = strings.TrimSpace(rec.Action[:idx])
			target = strings.TrimSpace(rec.Action[idx+1 : len(rec.Action)-1])
		}
	} else {
		class = ptyevent.ClassDecision // no named action: a question, never a permission
		subject, target = "question", rec.Question
	}

	caps := ExtractCapabilities(subject, target, rec.Trust)
	if class == ptyevent.ClassPermission && len(caps) == 0 {
		caps = []ptyevent.Capability{ptyevent.CapOther}
	}

	targetInWorkspace := IsTargetInWorkspace(target, workdir)

	options := make([]ptyevent.Option, len(rec.Options))
	semantics := make([]ptyevent.OptionSemantic, len(rec.Options))
	denyIdx := 0

	for i, lbl := range rec.Options {
		idx := i + 1
		sem := InferOptionSemantic(class, idx, lbl, len(rec.Options))
		if rec.Affirmative > 0 && idx == rec.Affirmative {
			if rec.Sticky {
				sem = ptyevent.SemAllowAlways
			} else {
				sem = ptyevent.SemAllowOnce
			}
		}
		if sem.Refuses() && denyIdx == 0 {
			denyIdx = idx
		}
		semantics[i] = sem

		scope := ptyevent.ScopeOnce
		if sem == ptyevent.SemAllowAlways {
			if rec.Trust {
				scope = ptyevent.ScopeWorkspace
			} else {
				scope = ptyevent.ScopeProject
			}
		} else if !sem.Allows() {
			scope = ""
		}

		options[i] = ptyevent.Option{
			Index:    idx,
			Label:    lbl,
			Semantic: sem,
			Scope:    scope,
			Default:  idx == loc.Selected,
		}
	}

	cancel := ptyevent.CancelPath{Key: "Escape"}
	if denyIdx > 0 {
		cancel.OptionIndex = denyIdx
	}

	fp := ptyevent.Fingerprint(backendID, 1, rec.Question, rec.Options)
	ik := ptyevent.InstanceKey(fp, subject, target)

	p := &ptyevent.Prompt{
		Fingerprint:       fp,
		ShapeVersion:      1,
		InstanceKey:       ik,
		Backend:           backendID,
		BackendVersion:    cliVersion,
		Class:             class,
		Capabilities:      caps,
		Subject:           subject,
		Target:            target,
		TargetInWorkspace: targetInWorkspace,
		Question:          rec.Question,
		Options:           options,
		CancelPath:        cancel,
		Navigate:          loc.Selected > 0,
		Provenance:        ptyevent.ProvFastBrain,
		Confidence:        rec.Confidence,
		Verified:          verified,
		Evidence: ptyevent.PromptEvidence{
			Raw: screen,
		},
	}

	if err := p.Validate(); err != nil {
		return nil, nil, err
	}

	var cand *ptyevent.LearnedShape
	if verified && rec.Confidence >= ptyevent.MinConfidenceFastBrain && class != ptyevent.ClassUnknown {
		cand = &ptyevent.LearnedShape{
			ID:           "shape-" + fp,
			Fingerprint:  fp,
			Backend:      backendID,
			CLIVersion:   cliVersion,
			ScopeKind:    ptyevent.ShapeScopeProject,
			ScopeID:      projectID,
			Status:       ptyevent.ShapeCandidate,
			Version:      1,
			Class:        class,
			Caps:         caps,
			Semantics:    semantics,
			Observations: 1,
			CreatedAt:    now,
		}
	}

	return p, cand, nil
}
