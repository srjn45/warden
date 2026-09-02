package autopilot

import (
	"fmt"
	"strings"

	"github.com/srjn45/warden/internal/store"
)

const (
	managerSlotSuffix  = "-autopilot"
	guardianSlotSuffix = "-guardian"
	maxSlotScopeLen    = 32 // store.ValidateName upper bound
)

var reservedPlanSuffixes = []string{managerSlotSuffix, guardianSlotSuffix}

// ManagerSlotID returns the stable manager session id for a slot scope.
func ManagerSlotID(scope string) string { return scope + managerSlotSuffix }

// GuardianSlotID returns the stable guardian session id for a slot scope.
func GuardianSlotID(scope string) string { return scope + guardianSlotSuffix }

func managerSlotIDOrEmpty(scope string) string {
	if scope == "" {
		return ""
	}
	return ManagerSlotID(scope)
}

func guardianSlotIDOrEmpty(scope string) string {
	if scope == "" {
		return ""
	}
	return GuardianSlotID(scope)
}

// SlotScope derives the stable scope token for manager/guardian slot ids from a
// run's display name. When another run already owns the base scope, a
// deterministic disambiguating suffix from runID is appended (e.g. default~a1b2c3).
func SlotScope(name, runID string, taken func(scope string) bool) (string, error) {
	base, err := sanitizeSlotScope(name)
	if err != nil {
		return "", err
	}
	scope := base
	if taken != nil && taken(scope) {
		scope = disambiguateScope(base, runID)
		if err := validateSlotScopeToken(scope); err != nil {
			return "", err
		}
	}
	return scope, nil
}

func validatePlanNameReservedSuffixes(name string) error {
	for _, suffix := range reservedPlanSuffixes {
		if strings.HasSuffix(name, suffix) {
			return fmt.Errorf("%w: plan name %q ends with reserved suffix %q", ErrRunConflict, name, suffix)
		}
	}
	return nil
}

func sanitizeSlotScope(name string) (string, error) {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-':
			b.WriteRune(r)
		case r == '.':
			b.WriteRune('-')
		default:
			b.WriteRune('-')
		}
	}
	scope := strings.Trim(b.String(), "-")
	if scope == "" {
		return "", fmt.Errorf("%w: plan name %q yields empty slot scope", ErrRunConflict, name)
	}
	if len(scope) > maxSlotScopeLen {
		scope = scope[:maxSlotScopeLen]
	}
	if err := validateSlotScopeToken(scope); err != nil {
		return "", fmt.Errorf("%w: plan name %q yields invalid slot scope: %v", ErrRunConflict, name, err)
	}
	return scope, nil
}

func validateSlotScopeToken(scope string) error {
	if err := store.ValidateName(scope); err != nil {
		return err
	}
	return store.SafeID(scope)
}

func runIDSuffix(runID string) string {
	hex := strings.TrimPrefix(runID, "ap-")
	if len(hex) > 6 {
		hex = hex[:6]
	}
	if hex == "" {
		hex = "000000"
	}
	return "_" + hex
}

func disambiguateScope(base, runID string) string {
	suffix := runIDSuffix(runID)
	maxBase := maxSlotScopeLen - len(suffix)
	if len(base) > maxBase {
		base = base[:maxBase]
	}
	return base + suffix
}
