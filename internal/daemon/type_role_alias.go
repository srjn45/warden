package daemon

import (
	"strings"

	"github.com/srjn45/warden/internal/store"
)

// roleFromDeprecatedType maps a legacy Agent.Type / SpawnRequest.type value onto
// a Role name for the one-release deprecation window. Role is canonical; type is
// accepted only when role is empty. Returns "" when typ is empty or unmapped
// (e.g. pipeline span-out/span-in), leaving role unset so the default general
// persona applies.
//
// Mapping (plan-213eaa87 / drop-agent-type-in-favor-of-role):
//
//	pr-review, code-review          → reviewer
//	development, code, docs, …      → implementer
//	analysis, spike, research, …    → general
func roleFromDeprecatedType(typ string) string {
	switch store.NormalizeType(strings.TrimSpace(typ)) {
	case store.TypePRReview, store.TypeCodeReview:
		return "reviewer"
	case store.TypeDevelopment, store.TypeCode, store.TypeDocs, store.TypeWebsite,
		store.TypeDebugCI, store.TypeTests, store.TypeMergePR, store.TypeRelease,
		store.TypeMonitorCI:
		return "implementer"
	case store.TypeAnalysis, store.TypeSpike, store.TypeResearch,
		store.TypeArchitecture, store.TypeDesign:
		return "general"
	default:
		return ""
	}
}

// resolveRoleCanonical returns the canonical role for a request: explicit role
// wins; otherwise a deprecated type is mapped. Empty means general at spawn.
func resolveRoleCanonical(role, typ string) string {
	if r := strings.TrimSpace(role); r != "" {
		return r
	}
	return roleFromDeprecatedType(typ)
}

// effectiveSessionRole returns the role a session should be classified under
// during the alias window: persisted Role if set, else a type→role mapping.
func effectiveSessionRole(role string, typ store.Type) string {
	if r := strings.TrimSpace(role); r != "" {
		return r
	}
	return roleFromDeprecatedType(string(typ))
}
