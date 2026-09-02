package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func legacyInventory(t *testing.T) []commandInventory {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "legacy_command_inventory.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inventory []commandInventory
	if err := json.Unmarshal(raw, &inventory); err != nil {
		t.Fatal(err)
	}
	return inventory
}

func TestLegacyCommandPathsRemainExecutable(t *testing.T) {
	t.Parallel()

	current := map[string]commandInventory{}
	for _, entry := range snapshotCommandInventory(newRootCmd()) {
		current[entry.Path] = entry
	}
	for _, want := range legacyInventory(t) {
		got, ok := current[want.Path]
		if !ok {
			t.Errorf("legacy path %q no longer exists", want.Path)
			continue
		}
		for _, field := range []struct {
			name      string
			want, got any
		}{
			{"use", want.Use, got.Use},
			{"runnable", want.Runnable, got.Runnable},
			{"json", want.JSON, got.JSON},
			{"aliases", want.Aliases, got.Aliases},
			{"local flags", want.LocalFlags, got.LocalFlags},
			{"inherited flags", want.InheritedFlags, got.InheritedFlags},
		} {
			if !reflect.DeepEqual(field.want, field.got) {
				t.Errorf("legacy path %q changed its %s: was %v, now %v", want.Path, field.name, field.want, field.got)
			}
		}
	}
}

func TestLegacyCommandPathsResolveThroughCobra(t *testing.T) {
	t.Parallel()

	root := newRootCmd()
	for _, entry := range legacyInventory(t) {
		if entry.Path == "." {
			continue
		}
		args := strings.Fields(entry.Path)
		cmd, remaining, err := root.Find(args)
		if err != nil {
			t.Errorf("legacy path %q does not resolve: %v", entry.Path, err)
			continue
		}
		if len(remaining) != 0 {
			t.Errorf("legacy path %q resolved only as far as %q (leftover %v)", entry.Path, cmd.CommandPath(), remaining)
		}
	}
}

func TestCompatibilityAliasesPointAtRealCommands(t *testing.T) {
	t.Parallel()

	root := newRootCmd()
	for _, alias := range CollectCompatibilityAliases(root) {
		if alias.Retained() {
			continue
		}
		args := strings.Fields(strings.TrimPrefix(alias.Canonical, "warden "))
		cmd, remaining, err := root.Find(args)
		if err != nil || len(remaining) != 0 {
			t.Errorf("alias %q points at %q, which does not resolve", alias.Path, alias.Canonical)
			continue
		}
		if cmd.CommandPath() != alias.Canonical {
			t.Errorf("alias %q points at %q, which resolves to %q", alias.Path, alias.Canonical, cmd.CommandPath())
		}
	}
}

func TestHelpAllListsEveryRelocatedLegacyPath(t *testing.T) {
	t.Parallel()

	root := newRootCmd()
	listed := map[string]bool{}
	for _, alias := range CollectCompatibilityAliases(root) {
		listed[alias.Path] = true
	}
	for _, entry := range legacyInventory(t) {
		if entry.Path == "." {
			continue
		}
		cmd, _, err := root.Find(strings.Fields(entry.Path))
		if err != nil {
			continue
		}
		if !cmd.Hidden || cmd.Annotations[AnnotationNodeKind] == NodeInternal {
			continue
		}
		if !listed["warden "+entry.Path] {
			t.Errorf("legacy path %q is hidden but missing from help --all", entry.Path)
		}
	}
}

func TestPermanentShortcutsAreExactlyTheApprovedSet(t *testing.T) {
	t.Parallel()

	want := map[string]bool{
		"ls": true, "start": true, "status": true, "send": true,
		"commit": true, "push": true, "sync": true, "check": true,
	}
	root := newRootCmd()
	got := map[string]bool{}
	WalkCommandTree(root, func(cmd *cobra.Command) {
		if cmd.Annotations[AnnotationAliasKind] == AliasPermanentShortcut {
			got[cmd.Name()] = true
		}
	})
	if !reflect.DeepEqual(want, got) {
		t.Errorf("permanent shortcuts = %v, want %v", keysOf(got), keysOf(want))
	}
	for name := range want {
		cmd, _, err := root.Find([]string{name})
		if err != nil {
			t.Errorf("permanent shortcut %q does not resolve: %v", name, err)
			continue
		}
		if cmd.Hidden {
			t.Errorf("permanent shortcut %q must stay visible", name)
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return sortedStrings(out)
}
