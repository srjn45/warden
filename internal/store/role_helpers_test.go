package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRoleFromDeprecatedType pins the canonical Type→Role mapping table shared
// across store/daemon/CLI during the one-release deprecation window.
func TestRoleFromDeprecatedType(t *testing.T) {
	cases := []struct {
		typ  string
		want string
	}{
		{"pr-review", "reviewer"},
		{"code-review", "reviewer"},
		{"development", "implementer"},
		{"code", "implementer"},
		{"docs", "implementer"},
		{"website", "implementer"},
		{"debug-ci", "implementer"},
		{"tests", "implementer"},
		{"merge-pr", "implementer"},
		{"release", "implementer"},
		{"monitor-ci", "implementer"},
		{"analysis", "general"},
		{"spike", "general"},
		{"research", "general"},
		{"architecture", "general"},
		{"design", "general"},
		{"", ""},
		{"span-out", ""},
		{"totally-unknown", ""},
	}
	for _, tc := range cases {
		t.Run(tc.typ, func(t *testing.T) {
			require.Equal(t, tc.want, RoleFromDeprecatedType(tc.typ))
		})
	}
}

func TestEffectiveRole(t *testing.T) {
	require.Equal(t, "worker", EffectiveRole("worker", TypeDevelopment), "persisted Role wins")
	require.Equal(t, "orchestrator", EffectiveRole("  orchestrator  ", TypePRReview), "whitespace trimmed")
	require.Equal(t, "implementer", EffectiveRole("", TypeDevelopment), "type→role when Role empty")
	require.Equal(t, "reviewer", EffectiveRole("", TypePRReview))
	require.Equal(t, "", EffectiveRole("", ""), "both empty → empty (callers pick DisplayRole)")
}

func TestDisplayRole(t *testing.T) {
	require.Equal(t, "worker", DisplayRole("worker", TypeDevelopment))
	require.Equal(t, "implementer", DisplayRole("", TypeDevelopment))
	require.Equal(t, "general", DisplayRole("", ""), "blank Role+Type defaults to general")
	require.Equal(t, "general", DisplayRole("", Type("span-out")), "unmapped type defaults to general")
}
