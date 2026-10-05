package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// TestFlagValueNamesHaveNoSpaces fails when a flag description contains a
// backticked phrase with whitespace (e.g. `warden start --help`): cobra renders
// the first backticked phrase as the flag's value name, so help would show
// "--aicli warden start --help" instead of a placeholder like "--aicli <ID>".
func TestFlagValueNamesHaveNoSpaces(t *testing.T) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		check := func(f *pflag.Flag) {
			name, _ := pflag.UnquoteUsage(f)
			if strings.ContainsAny(name, " \t") {
				t.Errorf("%s --%s: rendered value name %q contains a space; avoid backticks in the flag usage or use a placeholder like `<ID>`", c.CommandPath(), f.Name, name)
			}
		}
		c.LocalFlags().VisitAll(check)
		c.InheritedFlags().VisitAll(check)
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(newRootCmd())
}
