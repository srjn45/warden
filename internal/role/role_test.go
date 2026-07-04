package role

import (
	"strings"
	"testing"
)

func TestGetKnownRoles(t *testing.T) {
	// Every built-in role resolves, carries its name back, and has a description.
	for _, name := range []string{"general", "orchestrator", "implementer", "auto-merger", "reviewer"} {
		r, ok := Get(name)
		if !ok {
			t.Fatalf("Get(%q): want ok, got not found", name)
		}
		if r.Name != name {
			t.Errorf("Get(%q).Name = %q, want %q", name, r.Name, name)
		}
		if strings.TrimSpace(r.Description) == "" {
			t.Errorf("role %q has empty description", name)
		}
	}
}

func TestGetEmptyAndUnknown(t *testing.T) {
	// Empty name normalizes to the default (general).
	r, ok := Get("")
	if !ok {
		t.Fatalf(`Get(""): want ok, got not found`)
	}
	if r.Name != Default {
		t.Errorf(`Get("").Name = %q, want %q`, r.Name, Default)
	}
	// general carries no persona.
	if strings.TrimSpace(r.Persona) != "" {
		t.Errorf("general persona = %q, want empty", r.Persona)
	}
	// An unknown name is not found — the call site turns that into an error.
	if _, ok := Get("does-not-exist"); ok {
		t.Errorf("Get(unknown): want not found, got ok")
	}
}

func TestNamesDefaultFirstAndComplete(t *testing.T) {
	names := Names()
	if len(names) != 5 {
		t.Fatalf("Names() len = %d, want 5 (%v)", len(names), names)
	}
	if names[0] != Default {
		t.Errorf("Names()[0] = %q, want %q first", names[0], Default)
	}
	// The remainder (after Default) is sorted alphabetically.
	rest := names[1:]
	for i := 1; i < len(rest); i++ {
		if rest[i-1] > rest[i] {
			t.Errorf("Names() tail not sorted: %v", rest)
			break
		}
	}
	// All names resolve via Get.
	for _, n := range names {
		if _, ok := Get(n); !ok {
			t.Errorf("Names() lists %q but Get(%q) is not found", n, n)
		}
	}
}

func TestAllMatchesNames(t *testing.T) {
	all := All()
	names := Names()
	if len(all) != len(names) {
		t.Fatalf("All() len %d != Names() len %d", len(all), len(names))
	}
	for i, r := range all {
		if r.Name != names[i] {
			t.Errorf("All()[%d].Name = %q, want %q", i, r.Name, names[i])
		}
	}
}

func TestBuiltinDefaults(t *testing.T) {
	// Spot-check the default flags the built-in roles ship (the values spawn
	// resolution keys off).
	cases := map[string]Defaults{
		"general":      {},
		"orchestrator": {PermissionMode: "auto"},
		"implementer":  {Type: "development"},
		"auto-merger":  {PermissionMode: "auto", AutoApprove: true},
		"reviewer":     {Type: "pr-review"},
	}
	for name, want := range cases {
		r, ok := Get(name)
		if !ok {
			t.Fatalf("Get(%q): not found", name)
		}
		if r.Defaults.Type != want.Type {
			t.Errorf("%s Defaults.Type = %q, want %q", name, r.Defaults.Type, want.Type)
		}
		if r.Defaults.PermissionMode != want.PermissionMode {
			t.Errorf("%s Defaults.PermissionMode = %q, want %q", name, r.Defaults.PermissionMode, want.PermissionMode)
		}
		if r.Defaults.AutoApprove != want.AutoApprove {
			t.Errorf("%s Defaults.AutoApprove = %v, want %v", name, r.Defaults.AutoApprove, want.AutoApprove)
		}
	}
}

func TestNonGeneralHavePersona(t *testing.T) {
	// Every role except general injects a non-empty persona.
	for _, r := range All() {
		persona := strings.TrimSpace(r.Persona)
		if r.Name == Default {
			if persona != "" {
				t.Errorf("general persona = %q, want empty", persona)
			}
			continue
		}
		if persona == "" {
			t.Errorf("role %q has empty persona", r.Name)
		}
	}
}
