package stateclassifier

import (
	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/decisionrouter"
	"github.com/srjn45/warden/internal/ptyevent"
)

// ApprovalToPrompt maps a legacy agentbackend.Approval into a normalized,
// validated ptyevent.Prompt per contract spec §9.1–§9.7 using only generic
// inference. Callers that hold the Backend should call
// decisionrouter.NormalizeApproval so the adapter's PromptShaper applies.
func ApprovalToPrompt(app *agentbackend.Approval, backendID, screenExcerpt string, targetInWorkspace bool) (*ptyevent.Prompt, error) {
	return decisionrouter.NormalizeApproval(nil, app, decisionrouter.NormalizeContext{
		BackendID:         backendID,
		Screen:            screenExcerpt,
		TargetInWorkspace: targetInWorkspace,
	})
}
