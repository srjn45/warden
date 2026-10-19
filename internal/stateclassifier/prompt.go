package stateclassifier

import (
	"errors"
	"strings"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/ptyevent"
)

// ApprovalToPrompt maps a legacy agentbackend.Approval into a normalized,
// validated ptyevent.Prompt per contract spec §9.1–§9.7.
func ApprovalToPrompt(app *agentbackend.Approval, backendID, screenExcerpt string, targetInWorkspace bool) (*ptyevent.Prompt, error) {
	if app == nil {
		return nil, errors.New("approval is nil")
	}

	class := ptyevent.ClassPermission
	var caps []ptyevent.Capability
	subject := "CLI"
	target := app.Action

	if app.Kind == "trust" || app.Kind == agentbackend.ApprovalKindTrust {
		caps = append(caps, ptyevent.CapTrust)
		subject = "workspace"
		if target == "" {
			target = "trust_directory"
		}
	} else if app.Action != "" {
		// Tool permission
		if i := strings.Index(app.Action, ":"); i > 0 {
			subject = strings.TrimSpace(app.Action[:i])
			target = strings.TrimSpace(app.Action[i+1:])
		}
		c := inferCapabilities(subject, target)
		caps = append(caps, c...)
	} else if len(app.Options) > 0 {
		// No tool action, multiple options -> strategic/plan decision
		class = ptyevent.ClassDecision
	}

	if class == ptyevent.ClassPermission && len(caps) == 0 {
		caps = append(caps, ptyevent.CapOther)
	}

	// Build options (1-indexed)
	options := make([]ptyevent.Option, len(app.Options))
	var optionLabels []string
	denyIdx := 0

	for i, lbl := range app.Options {
		idx := i + 1
		optionLabels = append(optionLabels, lbl)
		sem := inferSemantic(class, idx, lbl, app)
		if sem.Refuses() && denyIdx == 0 {
			denyIdx = idx
		}
		scope := ptyevent.ScopeOnce
		if sem == ptyevent.SemAllowAlways {
			if capsHave(caps, ptyevent.CapTrust) {
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

	// Cancel path
	cancel := ptyevent.CancelPath{Key: "Escape"}
	if denyIdx > 0 {
		cancel.OptionIndex = denyIdx
	}

	fp := ptyevent.Fingerprint(backendID, 1, app.Question, optionLabels)
	ik := ptyevent.InstanceKey(fp, subject, target)

	p := &ptyevent.Prompt{
		Fingerprint:       fp,
		ShapeVersion:      1,
		InstanceKey:       ik,
		Backend:           backendID,
		Class:             class,
		Capabilities:      caps,
		Subject:           subject,
		Target:            target,
		TargetInWorkspace: targetInWorkspace,
		Question:          app.Question,
		Options:           options,
		CancelPath:        cancel,
		Navigate:          app.Navigate,
		Provenance:        ptyevent.ProvParser,
		Confidence:        1.0,
		Verified:          true,
		Evidence: ptyevent.PromptEvidence{
			Raw: screenExcerpt,
		},
	}

	if app.Inferred {
		p.Provenance = ptyevent.ProvFastBrain
		p.Confidence = 0.95
	}

	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func inferCapabilities(subject, target string) []ptyevent.Capability {
	s := strings.ToLower(subject)
	t := strings.ToLower(target)
	var caps []ptyevent.Capability

	switch {
	case strings.Contains(s, "bash") || strings.Contains(s, "command") || strings.Contains(s, "exec"):
		caps = append(caps, ptyevent.CapExecute)
		if strings.Contains(t, "rm ") || strings.Contains(t, "delete") || strings.Contains(t, "drop ") || strings.Contains(t, "--force") {
			caps = append(caps, ptyevent.CapDestructive)
		}
		if strings.Contains(t, "curl") || strings.Contains(t, "wget") || strings.Contains(t, "fetch") || strings.Contains(t, "http") {
			caps = append(caps, ptyevent.CapNetwork)
		}
		if strings.Contains(t, "git push") || strings.Contains(t, "npm publish") {
			caps = append(caps, ptyevent.CapPublish)
		}
	case strings.Contains(s, "edit") || strings.Contains(s, "write") || strings.Contains(s, "patch"):
		caps = append(caps, ptyevent.CapWrite)
	case strings.Contains(s, "read") || strings.Contains(s, "view") || strings.Contains(s, "glob"):
		caps = append(caps, ptyevent.CapRead)
	case strings.Contains(s, "web") || strings.Contains(s, "net") || strings.Contains(s, "url"):
		caps = append(caps, ptyevent.CapNetwork)
	default:
		caps = append(caps, ptyevent.CapExecute)
	}
	return caps
}

func inferSemantic(class ptyevent.PromptClass, idx int, label string, app *agentbackend.Approval) ptyevent.OptionSemantic {
	if class == ptyevent.ClassDecision {
		return ptyevent.SemChoice
	}
	l := strings.ToLower(label)
	if app.AffirmativeIdx > 0 && idx == app.AffirmativeIdx {
		if app.AffirmativeSticky {
			return ptyevent.SemAllowAlways
		}
		return ptyevent.SemAllowOnce
	}
	switch {
	case strings.Contains(l, "always") || strings.Contains(l, "remember") || strings.Contains(l, "trust this"):
		return ptyevent.SemAllowAlways
	case strings.Contains(l, "session"):
		return ptyevent.SemAllowSession
	case strings.Contains(l, "allow") || strings.Contains(l, "yes") || strings.Contains(l, "approve"):
		return ptyevent.SemAllowOnce
	case strings.Contains(l, "deny") || strings.Contains(l, "don't") || strings.Contains(l, "no") || strings.Contains(l, "refuse") || strings.Contains(l, "reject"):
		return ptyevent.SemDeny
	case strings.Contains(l, "cancel") || strings.Contains(l, "abort") || strings.Contains(l, "exit"):
		return ptyevent.SemCancel
	case strings.Contains(l, "feedback") || strings.Contains(l, "tell"):
		return ptyevent.SemDenyFeedback
	case strings.Contains(l, "type ") || strings.Contains(l, "freeform"):
		return ptyevent.SemFreeform
	default:
		if idx == 0 {
			return ptyevent.SemAllowOnce
		}
		if idx == len(app.Options)-1 && (strings.Contains(l, "don't") || strings.Contains(l, "reject")) {
			return ptyevent.SemDeny
		}
		return ptyevent.SemChoice
	}
}

func capsHave(caps []ptyevent.Capability, c ptyevent.Capability) bool {
	for _, cap := range caps {
		if cap == c {
			return true
		}
	}
	return false
}
