package store

import "testing"

// withLookup installs a custom-type resolver for the duration of a test and
// restores the previous one (the package var is process-global).
func withLookup(t *testing.T, m map[string]CustomTypePolicy) {
	t.Helper()
	prev := customTypeLookup
	SetCustomTypeLookup(func(name string) (CustomTypePolicy, bool) {
		p, ok := m[name]
		return p, ok
	})
	t.Cleanup(func() { customTypeLookup = prev })
}

// builtins is the exact built-in set, with each type's pre-#47 worktree default.
// The test below asserts these are byte-for-byte unchanged whether or not a
// custom-type lookup is installed.
var builtins = []struct {
	t        Type
	worktree bool
}{
	{TypeDevelopment, true},
	{TypeAnalysis, false},
	{TypeSpike, false},
	{TypePRReview, true},
	{TypeResearch, false},
	{TypeArchitecture, false},
	{TypeDesign, false},
	{TypeCodeReview, false},
	{TypeDocs, true},
	{TypeMonitorCI, false},
	{TypeDebugCI, true},
	{TypeMergePR, false},
	{TypeRelease, false},
}

func TestBuiltinsUnchangedWithoutPlugins(t *testing.T) {
	SetCustomTypeLookup(nil) // ensure no resolver
	for _, b := range builtins {
		if !b.t.Valid() {
			t.Errorf("%s should be Valid", b.t)
		}
		if !b.t.Builtin() {
			t.Errorf("%s should be Builtin", b.t)
		}
		if got := b.t.DefaultWorktree(); got != b.worktree {
			t.Errorf("%s DefaultWorktree = %v, want %v", b.t, got, b.worktree)
		}
		if got := NormalizeType(string(b.t)); got != b.t {
			t.Errorf("NormalizeType(%s) = %s, want %s", b.t, got, b.t)
		}
	}
}

func TestBuiltinsUnchangedWithPluginsRegistered(t *testing.T) {
	// A registered custom type must NOT alter any built-in's behavior.
	withLookup(t, map[string]CustomTypePolicy{"lint-bot": {Worktree: true}})
	for _, b := range builtins {
		if !b.t.Valid() {
			t.Errorf("%s should still be Valid", b.t)
		}
		if got := b.t.DefaultWorktree(); got != b.worktree {
			t.Errorf("%s DefaultWorktree = %v, want %v (built-in must be unaffected)", b.t, got, b.worktree)
		}
		if got := NormalizeType(string(b.t)); got != b.t {
			t.Errorf("NormalizeType(%s) = %s, want %s", b.t, got, b.t)
		}
	}
}

func TestCustomTypeValidatesAndGetsPolicy(t *testing.T) {
	withLookup(t, map[string]CustomTypePolicy{
		"lint-bot": {Worktree: true},
		"scratch":  {Worktree: false},
	})

	if !Type("lint-bot").Valid() {
		t.Error("registered custom type should be Valid")
	}
	if Type("lint-bot").Builtin() {
		t.Error("custom type must NOT report as Builtin")
	}
	if !Type("lint-bot").DefaultWorktree() {
		t.Error("lint-bot declared worktree isolation")
	}
	if Type("scratch").DefaultWorktree() {
		t.Error("scratch declared no worktree")
	}
	// NormalizeType preserves a registered custom type rather than collapsing it.
	if got := NormalizeType("lint-bot"); got != Type("lint-bot") {
		t.Errorf("NormalizeType(lint-bot) = %s, want lint-bot", got)
	}
}

func TestUnknownTypeStillRejectedAndCollapsed(t *testing.T) {
	withLookup(t, map[string]CustomTypePolicy{"lint-bot": {Worktree: true}})

	if Type("totally-unknown").Valid() {
		t.Error("an unregistered type must remain invalid")
	}
	if Type("totally-unknown").DefaultWorktree() {
		t.Error("an unregistered type must not get a worktree")
	}
	if got := NormalizeType("totally-unknown"); got != "" {
		t.Errorf("NormalizeType(unknown) = %s, want \"\"", got)
	}
}

func TestLegacyTypeMappingUnaffected(t *testing.T) {
	withLookup(t, map[string]CustomTypePolicy{"lint-bot": {Worktree: true}})
	// Legacy aliases must still map to their modern equivalents.
	if got := NormalizeType("buildkite-debug"); got != TypeDebugCI {
		t.Errorf("NormalizeType(buildkite-debug) = %s, want debug-ci", got)
	}
	if got := NormalizeType("test-run"); got != TypeTests {
		t.Errorf("NormalizeType(test-run) = %s, want tests", got)
	}
}
