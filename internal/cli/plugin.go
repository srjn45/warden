package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/plugin"
)

// formatPluginList renders the registered plugins for `wd plugin list`. It is
// pure over its inputs (config gate + specs) so it is unit-tested without a
// daemon, mirroring the rotate/insights thin-command style. enabled reflects the
// `plugins` config gate; when off, the registry is shown but flagged inactive so
// an operator can see what WOULD load. validate surfaces any config problems the
// loader would reject (blank/duplicate names, unknown events, colliding types).
func formatPluginList(enabled bool, specs []plugin.Spec) string {
	var b strings.Builder
	switch {
	case enabled:
		b.WriteString("plugin system: ENABLED\n")
	default:
		b.WriteString("plugin system: disabled (set `plugins.enabled: true` in config to activate)\n")
	}
	if len(specs) == 0 {
		b.WriteString("no plugins registered (plugins.registry is empty)\n")
		return b.String()
	}
	// Report config errors the daemon would reject, without aborting the listing.
	if _, err := plugin.Load(specs); err != nil {
		fmt.Fprintf(&b, "⚠ config errors (these plugins will NOT load): %v\n", err)
	}
	b.WriteString("\n")
	for _, s := range specs {
		fmt.Fprintf(&b, "%s\n", s.Name)
		fmt.Fprintf(&b, "  path:   %s\n", s.Path)
		fmt.Fprintf(&b, "  events: %s\n", formatEvents(s.Events))
		if len(s.TaskTypes) == 0 {
			b.WriteString("  task types: (none)\n")
		} else {
			b.WriteString("  task types:\n")
			for _, tt := range s.TaskTypes {
				iso := "in-repo"
				if tt.Worktree {
					iso = "worktree"
				}
				fmt.Fprintf(&b, "    %s (%s)\n", tt.Name, iso)
			}
		}
	}
	return b.String()
}

// formatEvents renders a plugin's subscribed events in the canonical order
// (AllEvents), so listings are stable regardless of config ordering.
func formatEvents(events []string) string {
	if len(events) == 0 {
		return "(none)"
	}
	set := map[string]bool{}
	for _, e := range events {
		set[strings.TrimSpace(e)] = true
	}
	var ordered []string
	for _, e := range plugin.AllEvents {
		if set[string(e)] {
			ordered = append(ordered, string(e))
			delete(set, string(e))
		}
	}
	// Append any unrecognized events last so a typo is still visible.
	var unknown []string
	for e := range set {
		unknown = append(unknown, e)
	}
	sort.Strings(unknown)
	ordered = append(ordered, unknown...)
	return strings.Join(ordered, ", ")
}

func newPluginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Inspect warden's plugin system (#47): custom task types + lifecycle hooks",
		Long: "Plugins are external executables registered in config (plugins.registry) that " +
			"declare custom agent task types and subscribe to lifecycle hook events (pre/post " +
			"spawn, commit, check, teardown), invoked over a JSON-over-stdio protocol. The " +
			"system is OFF by default — plugins run external code — and every hook fails open " +
			"(a broken/slow/missing plugin is logged and skipped, never blocking an agent).",
	}
	cmd.AddCommand(newPluginListCmd())
	return cmd
}

func newPluginListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered plugins, their custom task types, and subscribed hook events",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := config.Load(configPathFor(cmd))
			fmt.Fprint(cmd.OutOrStdout(), formatPluginList(cfg.GetPluginsEnabled(), cfg.GetPlugins()))
			return nil
		},
	}
}
