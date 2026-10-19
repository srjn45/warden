package decisionrouter

import (
	"errors"
	"strings"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/ptyevent"
)

// NormalizeContext is what normalization needs beyond the parsed Approval.
type NormalizeContext struct {
	BackendID  string
	CLIVersion string
	// Screen is the screen the approval was parsed from. It is redacted before
	// it becomes evidence.
	Screen            string
	Workdir           string
	TargetInWorkspace bool
}

// ambiguousConfidence is what a menu gets when its meaning had to be guessed.
// It is below every provenance floor, so Confident() fails and the state
// machine routes the prompt to needs_attention.
const ambiguousConfidence = 0.5

// NormalizeApproval is the single path from a backend parser's Approval to the
// normalized Prompt contract (§9.7). When the backend implements
// agentbackend.PromptShaper and its shape validates against the approval, the
// adapter's statement of subject, capabilities, option semantics and cancel path
// is authoritative. Otherwise generic wording inference applies, and a
// permission menu whose options needed a positional guess is marked ambiguous.
func NormalizeApproval(b agentbackend.Backend, app *agentbackend.Approval, nc NormalizeContext) (*ptyevent.Prompt, error) {
	if app == nil {
		return nil, errors.New("approval is nil")
	}
	backendID := nc.BackendID
	if backendID == "" && b != nil {
		backendID = b.ID()
	}

	var (
		prompt *ptyevent.Prompt
		err    error
	)
	if sh, ok := shapeFor(b, app); ok {
		prompt = promptFromShape(sh, app, backendID, nc)
	} else {
		prompt = promptFromHeuristics(app, backendID, nc)
	}
	redactEvidence(prompt, nc.Screen)
	if err = prompt.Validate(); err != nil {
		return nil, err
	}
	return prompt, nil
}

func shapeFor(b agentbackend.Backend, app *agentbackend.Approval) (agentbackend.PromptShape, bool) {
	shaper, ok := b.(agentbackend.PromptShaper)
	if !ok {
		return agentbackend.PromptShape{}, false
	}
	sh, ok := shaper.ShapePrompt(app)
	if !ok || agentbackend.ValidateShape(sh, app) != nil {
		return agentbackend.PromptShape{}, false
	}
	return sh, true
}

func promptFromShape(sh agentbackend.PromptShape, app *agentbackend.Approval, backendID string, nc NormalizeContext) *ptyevent.Prompt {
	caps := sh.Caps
	if sh.Class == ptyevent.ClassPermission && !hasCap(caps, ptyevent.CapTrust) {
		// The adapter's capabilities are a floor; target-derived heuristics may
		// only add (`git push` is publish+network), never remove.
		caps = unionCaps(caps, ExtractCapabilities(sh.Subject, sh.Target, false))
	}

	options := make([]ptyevent.Option, len(app.Options))
	labels := make([]string, len(app.Options))
	for i, label := range app.Options {
		labels[i] = label
		options[i] = ptyevent.Option{
			Index:    i + 1,
			Label:    label,
			Key:      sh.Options[i].Key,
			Semantic: sh.Options[i].Semantic,
			Scope:    sh.Options[i].Scope,
			Default:  i+1 == app.SelectedIdx,
		}
	}
	cancel := sh.Cancel
	if cancel.Key == "" && cancel.OptionIndex == 0 {
		cancel.Key = "Escape"
	}

	fp := ptyevent.Fingerprint(backendID, 1, app.Question, labels)
	return &ptyevent.Prompt{
		Fingerprint:       fp,
		ShapeVersion:      1,
		InstanceKey:       ptyevent.InstanceKey(fp, sh.Subject, sh.Target),
		Backend:           backendID,
		BackendVersion:    nc.CLIVersion,
		Class:             sh.Class,
		Capabilities:      caps,
		Subject:           sh.Subject,
		Target:            sh.Target,
		TargetInWorkspace: targetInWorkspace(sh.Subject == "workspace", sh.Target, nc),
		Question:          app.Question,
		Options:           options,
		CancelPath:        cancel,
		Navigate:          app.Navigate,
		Provenance:        ptyevent.ProvParser,
		Confidence:        1.0,
		Verified:          true,
	}
}

func promptFromHeuristics(app *agentbackend.Approval, backendID string, nc NormalizeContext) *ptyevent.Prompt {
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
	} else {
		// A permission asks to do something specific. A menu that names no
		// action is a question (plan approval, "which approach"), whatever its
		// "Yes" option says: it is never answered by a posture.
		class = ptyevent.ClassDecision
		// The instance key must separate two different questions that share a
		// shape, so TOCTOU catches a swapped question before a brain answer lands.
		subject, target = "question", app.Question
	}

	caps := ExtractCapabilities(subject, target, isTrust)
	if class == ptyevent.ClassPermission && len(caps) == 0 {
		caps = []ptyevent.Capability{ptyevent.CapOther}
	}

	options := make([]ptyevent.Option, len(app.Options))
	labels := make([]string, len(app.Options))
	denyIdx := 0
	ambiguous := false
	for i, lbl := range app.Options {
		idx := i + 1
		labels[i] = lbl
		sem, positional := inferOptionSemantic(class, idx, lbl, len(app.Options))
		if app.AffirmativeIdx > 0 && idx == app.AffirmativeIdx {
			positional = false
			if app.AffirmativeSticky {
				sem = ptyevent.SemAllowAlways
			} else {
				sem = ptyevent.SemAllowOnce
			}
		}
		if positional && class == ptyevent.ClassPermission {
			ambiguous = true
		}
		if sem.Refuses() && denyIdx == 0 {
			denyIdx = idx
		}
		scope := ptyevent.ScopeOnce
		switch {
		case sem == ptyevent.SemAllowAlways && isTrust:
			scope = ptyevent.ScopeWorkspace
		case sem == ptyevent.SemAllowAlways:
			scope = ptyevent.ScopeProject
		case sem == ptyevent.SemAllowSession:
			scope = ptyevent.ScopeSession
		case !sem.Allows():
			scope = ""
		}
		options[i] = ptyevent.Option{Index: idx, Label: lbl, Semantic: sem, Scope: scope, Default: idx == app.SelectedIdx}
	}

	cancel := ptyevent.CancelPath{Key: "Escape"}
	if denyIdx > 0 {
		cancel.OptionIndex = denyIdx
	}

	prov, conf := ptyevent.ProvParser, 1.0
	if app.Inferred {
		prov, conf = ptyevent.ProvFastBrain, 0.95
	}
	if ambiguous {
		conf = ambiguousConfidence
	}

	fp := ptyevent.Fingerprint(backendID, 1, app.Question, labels)
	return &ptyevent.Prompt{
		Fingerprint:       fp,
		ShapeVersion:      1,
		InstanceKey:       ptyevent.InstanceKey(fp, subject, target),
		Backend:           backendID,
		BackendVersion:    nc.CLIVersion,
		Class:             class,
		Capabilities:      caps,
		Subject:           subject,
		Target:            target,
		TargetInWorkspace: targetInWorkspace(isTrust, target, nc),
		Question:          app.Question,
		Options:           options,
		CancelPath:        cancel,
		Navigate:          app.Navigate,
		Provenance:        prov,
		Confidence:        conf,
		Verified:          true,
	}
}

// targetInWorkspace honors an explicit adapter/caller verdict and otherwise
// resolves the target against the session worktree. A trust target that cannot
// be resolved (no worktree known) is NOT assumed to be inside it.
func targetInWorkspace(isTrust bool, target string, nc NormalizeContext) bool {
	if nc.TargetInWorkspace {
		return true
	}
	if nc.Workdir == "" || target == "" {
		return !isTrust
	}
	return IsTargetInWorkspace(target, nc.Workdir)
}

// redactEvidence stores the redacted screen as the prompt's evidence. Every
// string taken from the screen (question, labels, subject, target) is redacted
// too, so the Prompt never carries a secret that scrolled past in the pane.
func redactEvidence(p *ptyevent.Prompt, screen string) {
	raw, reds := ptyevent.Redact([]byte(screen))
	p.Evidence.Raw, p.Evidence.Redactions = string(raw), reds
	p.Question = redactString(p.Question)
	p.Subject = redactString(p.Subject)
	p.Target = redactString(p.Target)
	for i := range p.Options {
		p.Options[i].Label = redactString(p.Options[i].Label)
	}
}

func redactString(s string) string {
	if s == "" {
		return s
	}
	out, _ := ptyevent.Redact([]byte(s))
	return string(out)
}

func hasCap(caps []ptyevent.Capability, c ptyevent.Capability) bool {
	for _, x := range caps {
		if x == c {
			return true
		}
	}
	return false
}

func unionCaps(a, b []ptyevent.Capability) []ptyevent.Capability {
	order := []ptyevent.Capability{
		ptyevent.CapRead, ptyevent.CapWrite, ptyevent.CapExecute, ptyevent.CapNetwork,
		ptyevent.CapCredential, ptyevent.CapPublish, ptyevent.CapDestructive, ptyevent.CapTrust, ptyevent.CapOther,
	}
	var out []ptyevent.Capability
	for _, c := range order {
		if hasCap(a, c) || hasCap(b, c) {
			out = append(out, c)
		}
	}
	// `other` means "unmapped"; once a concrete capability is known it is noise.
	if len(out) > 1 && hasCap(out, ptyevent.CapOther) {
		trimmed := out[:0]
		for _, c := range out {
			if c != ptyevent.CapOther {
				trimmed = append(trimmed, c)
			}
		}
		out = trimmed
	}
	return out
}
