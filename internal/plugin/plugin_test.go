package plugin

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/store"
)

func TestLoadEmpty(t *testing.T) {
	r, err := Load(nil)
	require.NoError(t, err)
	require.Empty(t, r.Plugins())
	_, ok := r.Lookup("anything")
	require.False(t, ok)
}

func TestLoadHappyPath(t *testing.T) {
	r, err := Load([]Spec{{
		Name:   "notifier",
		Path:   "/usr/local/bin/notifier",
		Events: []string{"post-commit", "post-spawn"},
		TaskTypes: []TaskTypeSpec{
			{Name: "lint-bot", Worktree: true},
			{Name: "scratch", Worktree: false},
		},
	}})
	require.NoError(t, err)
	require.Len(t, r.Plugins(), 1)

	p := r.Plugins()[0]
	require.Equal(t, "notifier", p.Name)
	require.True(t, p.Subscribes(EventPostCommit))
	require.True(t, p.Subscribes(EventPostSpawn))
	require.False(t, p.Subscribes(EventPreSpawn))

	pol, ok := r.Lookup("lint-bot")
	require.True(t, ok)
	require.True(t, pol.Worktree, "lint-bot declared worktree isolation")

	pol, ok = r.Lookup("scratch")
	require.True(t, ok)
	require.False(t, pol.Worktree)

	_, ok = r.Lookup("nope")
	require.False(t, ok)
}

func TestLoadRejectsBadConfig(t *testing.T) {
	cases := map[string][]Spec{
		"empty name":           {{Name: "", Path: "/x"}},
		"empty path":           {{Name: "p", Path: ""}},
		"duplicate name":       {{Name: "p", Path: "/a"}, {Name: "p", Path: "/b"}},
		"unknown event":        {{Name: "p", Path: "/a", Events: []string{"on-tuesday"}}},
		"builtin collision":    {{Name: "p", Path: "/a", TaskTypes: []TaskTypeSpec{{Name: "development"}}}},
		"empty task-type name": {{Name: "p", Path: "/a", TaskTypes: []TaskTypeSpec{{Name: " "}}}},
	}
	for name, specs := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(specs)
			require.Error(t, err)
		})
	}
}

func TestLoadRejectsDuplicateCustomTypeAcrossPlugins(t *testing.T) {
	_, err := Load([]Spec{
		{Name: "a", Path: "/a", TaskTypes: []TaskTypeSpec{{Name: "lint-bot"}}},
		{Name: "b", Path: "/b", TaskTypes: []TaskTypeSpec{{Name: "lint-bot"}}},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "already registered")
}

func TestLoadTrimsWhitespace(t *testing.T) {
	r, err := Load([]Spec{{
		Name:      "  notifier  ",
		Path:      "  /bin/x  ",
		Events:    []string{" post-commit "},
		TaskTypes: []TaskTypeSpec{{Name: " lint-bot ", Worktree: true}},
	}})
	require.NoError(t, err)
	require.Equal(t, "notifier", r.Plugins()[0].Name)
	require.Equal(t, "/bin/x", r.Plugins()[0].Path)
	require.True(t, r.Plugins()[0].Subscribes(EventPostCommit))
	_, ok := r.Lookup("lint-bot")
	require.True(t, ok)
}

// TestLookupSatisfiesStoreSeam confirms a Registry's Lookup has the exact
// signature store.SetCustomTypeLookup expects — a compile-time guarantee the seam
// stays wired (mirrors rotate's `var _ rotator = (*client.Client)(nil)`).
func TestLookupSatisfiesStoreSeam(t *testing.T) {
	r, err := Load([]Spec{{Name: "p", Path: "/x", TaskTypes: []TaskTypeSpec{{Name: "lint-bot", Worktree: true}}}})
	require.NoError(t, err)
	var fn func(string) (store.CustomTypePolicy, bool) = r.Lookup
	pol, ok := fn("lint-bot")
	require.True(t, ok)
	require.True(t, pol.Worktree)
}

func TestNilRegistrySafe(t *testing.T) {
	var r *Registry
	require.Nil(t, r.Plugins())
	_, ok := r.Lookup("x")
	require.False(t, ok)
	require.Nil(t, r.subscribers(EventPreSpawn))
}
