package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// commandInventory is the contract snapshot taken before the command/help
// redesign. It deliberately describes Cobra's actual fresh tree rather than a
// second hand-maintained command list.
type commandInventory struct {
	Path           string   `json:"path"`
	Use            string   `json:"use"`
	Aliases        []string `json:"aliases,omitempty"`
	Hidden         bool     `json:"hidden,omitempty"`
	Runnable       bool     `json:"runnable,omitempty"`
	LocalFlags     []string `json:"local_flags,omitempty"`
	InheritedFlags []string `json:"inherited_flags,omitempty"`
	JSON           bool     `json:"json,omitempty"`
}

func TestCurrentCommandInventory(t *testing.T) {
	t.Parallel()

	wantPath := filepath.Join("testdata", "current_command_inventory.json")
	got := snapshotCommandInventory(newRootCmd())
	encoded, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')

	if os.Getenv("UPDATE_COMMAND_INVENTORY") == "1" {
		if err := os.WriteFile(wantPath, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(encoded, want) {
		t.Fatalf("current Cobra tree changed without an inventory decision; run UPDATE_COMMAND_INVENTORY=1 go test ./internal/cli -run TestCurrentCommandInventory only after reviewing the semantic contract\n--- want\n%s\n--- got\n%s", want, encoded)
	}
}

func snapshotCommandInventory(root *cobra.Command) []commandInventory {
	var inventory []commandInventory
	var visit func(*cobra.Command)
	visit = func(cmd *cobra.Command) {
		entry := commandInventory{
			Path:           strings.TrimSpace(strings.TrimPrefix(cmd.CommandPath(), root.Name())),
			Use:            cmd.Use,
			Aliases:        sortedStrings(cmd.Aliases),
			Hidden:         cmd.Hidden,
			Runnable:       cmd.Runnable(),
			LocalFlags:     localFlagNames(cmd),
			InheritedFlags: flagNames(cmd.InheritedFlags()),
		}
		if entry.Path == "" {
			entry.Path = "."
		}
		entry.JSON = cmd.Flags().Lookup("json") != nil || cmd.InheritedFlags().Lookup("json") != nil
		inventory = append(inventory, entry)

		children := append([]*cobra.Command(nil), cmd.Commands()...)
		sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
		for _, child := range children {
			visit(child)
		}
	}
	visit(root)
	return inventory
}

func flagNames(flags *pflag.FlagSet) []string {
	var names []string
	flags.VisitAll(func(flag *pflag.Flag) { names = append(names, flag.Name) })
	return names
}

func localFlagNames(cmd *cobra.Command) []string {
	names := append(flagNames(cmd.LocalNonPersistentFlags()), flagNames(cmd.PersistentFlags())...)
	sort.Strings(names)
	return names
}

func sortedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}
