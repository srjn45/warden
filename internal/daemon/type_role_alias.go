package daemon

import (
	"strings"

	"github.com/srjn45/warden/internal/store"
)

// roleFromDeprecatedType delegates to store.RoleFromDeprecatedType, the
// canonical mapping table shared with the store layer.
func roleFromDeprecatedType(typ string) string {
	return store.RoleFromDeprecatedType(typ)
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
	return store.EffectiveRole(role, typ)
}
