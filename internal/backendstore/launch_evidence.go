package backendstore

import (
	"fmt"
	"strings"
	"time"
)

// ClassifyLaunchFailure recognizes only unambiguous model-selection failures.
// In particular, a generic non-zero exit is not negative catalog evidence: it
// can be caused by a network outage, login expiry, a trust prompt, or a bad
// reasoning flag. Keep this conservative; a false exclusion is worse than a
// missed optimization because it removes a usable model from automatic routing.
func ClassifyLaunchFailure(output string) LaunchFailureKind {
	s := strings.ToLower(strings.TrimSpace(output))
	if s == "" {
		return LaunchFailureUnknown
	}
	if strings.Contains(s, "429") || strings.Contains(s, "rate limit") || strings.Contains(s, "quota exceeded") {
		return LaunchFailureQuota
	}
	if strings.Contains(s, "unauthorized") || strings.Contains(s, "authentication") || strings.Contains(s, "login required") || strings.Contains(s, "not logged in") {
		return LaunchFailureAuthentication
	}
	if strings.Contains(s, "timeout") || strings.Contains(s, "timed out") || strings.Contains(s, "connection refused") || strings.Contains(s, "network") || strings.Contains(s, "dns") {
		return LaunchFailureNetwork
	}
	if strings.Contains(s, "reasoning effort") || strings.Contains(s, "reasoning level") || strings.Contains(s, "unsupported effort") {
		return LaunchFailureReasoning
	}
	if strings.Contains(s, "trust this directory") || strings.Contains(s, "trust the workspace") || strings.Contains(s, "do you trust") {
		return LaunchFailureTrustPrompt
	}
	// These phrases are intentionally paired with a model-oriented noun. A
	// backend-wide "unsupported" must not poison every model in its catalog.
	if (strings.Contains(s, "model") || strings.Contains(s, "--model")) &&
		(strings.Contains(s, "not found") || strings.Contains(s, "does not exist") || strings.Contains(s, "unsupported") || strings.Contains(s, "not available") || strings.Contains(s, "not entitled") || strings.Contains(s, "access denied") || strings.Contains(s, "removed")) {
		return LaunchFailureInvalidModel
	}
	return LaunchFailureOther
}

// RecordLaunchEvidence applies a launch observation to the active account
// scope. Invalid-model evidence creates a bounded, model-only exclusion; it
// never changes Backend.LimitedUntil. A later successful launch is positive
// evidence and clears an older exclusion immediately.
func (s *Store) RecordLaunchEvidence(aiCli, modelID string, evidence LaunchEvidence, rejectFor time.Duration) error {
	if strings.TrimSpace(aiCli) == "" || strings.TrimSpace(modelID) == "" {
		return fmt.Errorf("launch evidence requires AI CLI and model ID")
	}
	if evidence.ObservedAt.IsZero() {
		evidence.ObservedAt = time.Now().UTC()
	}
	scope, err := s.GetCatalogScope(aiCli)
	if err != nil {
		return fmt.Errorf("get active catalog scope: %w", err)
	}
	if scope.ActiveScopeFP == "" {
		return fmt.Errorf("active catalog scope is not known for %s", aiCli)
	}

	o, err := s.GetCatalogObservation(aiCli, scope.ActiveScopeFP, modelID)
	if err != nil && err != ErrNotFound {
		return err
	}
	if o.AiCli == "" {
		o = StoredObservation{AiCli: aiCli, ScopeFP: scope.ActiveScopeFP, ModelID: modelID}
	}
	o.LastCheckedAt = evidence.ObservedAt
	if evidence.Kind == "" {
		evidence.Kind = LaunchFailureUnknown
	}
	if evidence.Kind == LaunchFailureInvalidModel && evidence.PreTask {
		if rejectFor <= 0 {
			rejectFor = 30 * time.Minute
		}
		o.Availability = AvailabilityUnavailable
		o.LaunchRejectedUntil = evidence.ObservedAt.Add(rejectFor)
		o.LaunchRejectionReason = string(evidence.Kind)
	} else if evidence.Succeeded && evidence.PreTask {
		// Successful pre-task startup is the only positive launch signal. It
		// supersedes a stale false negative for this exact scope/model.
		o.Availability = AvailabilityAvailable
		o.LastSeenAt = evidence.ObservedAt
		o.LaunchRejectedUntil = time.Time{}
		o.LaunchRejectionReason = ""
	}
	return s.PutCatalogObservation(o)
}
