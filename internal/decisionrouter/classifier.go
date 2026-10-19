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
	fastbrain  interface {
		Decide(ctx context.Context, req fastbrain.Request) (fastbrain.Response, error)
	}
}

// NewPromptClassifier creates a new PromptClassifier.
func NewPromptClassifier(store LearnedShapeStore, fb interface {
	Decide(ctx context.Context, req fastbrain.Request) (fastbrain.Response, error)
}) *PromptClassifier {
	return &PromptClassifier{
		shapeStore: store,
		fastbrain:  fb,
	}
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
			prompt, err := c.approvalToPrompt(app, backendID, req.CLIVersion, screen, req.Workdir, req.TargetInWorkspace)
			if err == nil && prompt != nil && prompt.Validate() == nil {
				return ClassifyResult{
					Prompt:    prompt,
					RawScreen: screen,
				}, nil
			}
		}
	}

	// 3. Fallback / Provider CLI format adaptation: Fast-Brain prompt recognition + Learned Shape check
	if c.fastbrain != nil {
		rec, recognized, err := fastbrain.RecognizePromptFor(ctx, c.fastbrain, screen, req.AgentID)
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
				if candShape != nil && c.shapeStore != nil {
					_, _ = c.shapeStore.AddCandidate(*candShape)
				}
				return ClassifyResult{
					Prompt:         prompt,
					CandidateShape: candShape,
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

func (c *PromptClassifier) approvalToPrompt(app *agentbackend.Approval, backendID, cliVersion, screen, workdir string, explicitTargetInWorkspace bool) (*ptyevent.Prompt, error) {
	isTrust := app.Kind == agentbackend.ApprovalKindTrust || app.Kind == "trust"

	class := ptyevent.ClassPermission
	subject := "CLI"
	target := app.Action

	if isTrust {
		subject = "workspace"
		if target == "" {
			target = "trust_directory"
		}
	} else if app.Action != "" {
		if idx := strings.Index(app.Action, "("); idx > 0 && strings.HasSuffix(app.Action, ")") {
			subject = strings.TrimSpace(app.Action[:idx])
			target = strings.TrimSpace(app.Action[idx+1 : len(app.Action)-1])
		} else if idx := strings.Index(app.Action, ":"); idx > 0 {
			subject = strings.TrimSpace(app.Action[:idx])
			target = strings.TrimSpace(app.Action[idx+1:])
		}
	} else if len(app.Options) > 1 && app.AffirmativeIdx == 0 {
		class = ptyevent.ClassDecision
	}

	caps := ExtractCapabilities(subject, target, isTrust)
	if class == ptyevent.ClassPermission && len(caps) == 0 {
		caps = []ptyevent.Capability{ptyevent.CapOther}
	}

	targetInWorkspace := explicitTargetInWorkspace
	if !explicitTargetInWorkspace && workdir != "" && target != "" {
		targetInWorkspace = IsTargetInWorkspace(target, workdir)
	}

	// Build options
	options := make([]ptyevent.Option, len(app.Options))
	optionLabels := make([]string, len(app.Options))
	denyIdx := 0

	for i, lbl := range app.Options {
		idx := i + 1
		optionLabels[i] = lbl
		sem := InferOptionSemantic(class, idx, lbl, len(app.Options))
		if app.AffirmativeIdx > 0 && idx == app.AffirmativeIdx {
			if app.AffirmativeSticky {
				sem = ptyevent.SemAllowAlways
			} else {
				sem = ptyevent.SemAllowOnce
			}
		}
		if sem.Refuses() && denyIdx == 0 {
			denyIdx = idx
		}

		scope := ptyevent.ScopeOnce
		if sem == ptyevent.SemAllowAlways {
			if isTrust {
				scope = ptyevent.ScopeWorkspace
			} else {
				scope = ptyevent.ScopeProject
			}
		} else if sem == ptyevent.SemAllowSession {
			scope = ptyevent.ScopeSession
		} else if !sem.Allows() {
			scope = ""
		}

		options[i] = ptyevent.Option{
			Index:    idx,
			Label:    lbl,
			Semantic: sem,
			Scope:    scope,
			Default:  idx == app.SelectedIdx,
		}
	}

	cancel := ptyevent.CancelPath{Key: "Escape"}
	if denyIdx > 0 {
		cancel.OptionIndex = denyIdx
	}

	fp := ptyevent.Fingerprint(backendID, 1, app.Question, optionLabels)
	ik := ptyevent.InstanceKey(fp, subject, target)

	prov := ptyevent.ProvParser
	conf := 1.0
	if app.Inferred {
		prov = ptyevent.ProvFastBrain
		conf = 0.95
	}

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
		Question:          app.Question,
		Options:           options,
		CancelPath:        cancel,
		Navigate:          app.Navigate,
		Provenance:        prov,
		Confidence:        conf,
		Verified:          true,
		Evidence: ptyevent.PromptEvidence{
			Raw: screen,
		},
	}

	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
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
	} else if len(rec.Options) > 1 && rec.Affirmative == 0 {
		class = ptyevent.ClassDecision
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
