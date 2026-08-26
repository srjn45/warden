package store

// CustomTypePolicy is the isolation policy a plugin-provided (non-built-in) task
// type declares (#47). It carries exactly what the closed-enum logic needs to
// treat an unknown type correctly: whether spawning it creates a git worktree by
// default. Built-in types never consult this — their behavior is unchanged.
type CustomTypePolicy struct {
	// Worktree mirrors Type.DefaultWorktree for a custom type: true ⇒ this type
	// is isolated in its own worktree by default (like the write-agent built-ins).
	Worktree bool
}

// customTypeLookup is the seam by which store's closed-enum logic (Valid,
// DefaultWorktree, NormalizeType) consults plugin-registered task types WITHOUT
// importing the plugin package (which imports store). It is nil by default —
// meaning no plugins, so the enum behaves exactly as it always has — and the
// daemon sets it once at startup via SetCustomTypeLookup when the plugin gate is
// on. A single function var (not a mutable map) keeps the seam simple and
// race-free: it is written once before any agent is served and only read after.
var customTypeLookup func(name string) (CustomTypePolicy, bool)

// SetCustomTypeLookup installs the resolver store consults for non-built-in type
// names. Pass nil to clear it (the default, plugins-off behavior). The plugin
// registry's Lookup method is the intended argument.
func SetCustomTypeLookup(fn func(name string) (CustomTypePolicy, bool)) {
	customTypeLookup = fn
}

// lookupCustomType resolves a custom type's policy, or (zero,false) when no
// resolver is installed or the name is not a registered custom type.
func lookupCustomType(name string) (CustomTypePolicy, bool) {
	if customTypeLookup == nil {
		return CustomTypePolicy{}, false
	}
	return customTypeLookup(name)
}

// Builtin reports whether t is one of warden's built-in task types (i.e. not a
// plugin-provided custom type). It is the exhaustive switch's source of truth,
// reused by Valid and by the plugin loader to reject a custom type that would
// shadow a built-in.
func (t Type) Builtin() bool {
	for _, b := range Builtin() {
		if t == b {
			return true
		}
	}
	return false
}
