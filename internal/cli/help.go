package cli

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Annotation keys are the shared contract used by runtime help, generated
// documentation, completion, and namespace/compatibility command factories.
const (
	AnnotationHelpGroup       = "warden.help.group"
	AnnotationHelpOrder       = "warden.help.order"
	AnnotationCanonicalPath   = "warden.help.canonical-path"
	AnnotationAliasKind       = "warden.help.alias-kind"
	AnnotationNodeKind        = "warden.help.node-kind"
	AnnotationIncludeInAll    = "warden.help.include-all"
	AnnotationIncludeInDocs   = "warden.help.include-docs"
	AnnotationIncludeComplete = "warden.help.include-completion"
	AnnotationDeprecationID   = "warden.help.deprecation-id"

	AliasCompatibility     = "compatibility"
	AliasPermanentShortcut = "permanent-shortcut"

	NodeNamespace  = "namespace"
	NodeEntryPoint = "entry-point"
	NodeLeaf       = "leaf"
	NodeInternal   = "internal-hook"
)

type helpGroup struct {
	ID, Title string
	Order     int
}

var helpGroups = []helpGroup{
	{"run", "Run work", 10},
	{"project", "Work with a project", 20},
	{"coordinate", "Coordinate", 30},
	{"observe", "Observe and configure", 40},
	{"operate", "Operate warden", 50},
	{"entry", "Get started and interact", 60},
	{"shortcut", "Shortcuts", 70},
}

var rootHelpPlacement = map[string]struct {
	group string
	order int
}{
	"start": {"shortcut", 10}, "ls": {"shortcut", 20}, "status": {"shortcut", 30}, "send": {"shortcut", 40},
	"commit": {"shortcut", 50}, "push": {"shortcut", 60}, "sync": {"shortcut", 70},
	"pipeline": {"run", 20}, "autopilot": {"run", 30}, "schedule": {"run", 40},
	"project": {"project", 5}, "workspace": {"project", 8},
	"adopt": {"run", 100}, "attach": {"run", 110}, "delete": {"run", 120}, "digest": {"run", 130},
	"done": {"run", 140}, "force-compact": {"run", 150}, "fork": {"run", 160}, "handoff": {"run", 170},
	"land": {"run", 180}, "recover": {"run", 190}, "remove-worktree": {"run", 200}, "restore": {"run", 210},
	"role": {"run", 220}, "rotate": {"run", 230}, "set-permission-mode": {"run", 240}, "set-role": {"run", 250},
	"stop": {"run", 260}, "switch": {"run", 270}, "tail": {"run", 280}, "terminate": {"run", 290},
	"worktree": {"project", 10}, "snapshot": {"project", 20}, "memory": {"project", 30}, "preset": {"project", 40},
	"prompt-template": {"project", 50}, "library": {"project", 60}, "plugin": {"project", 70}, "branches": {"project", 80},
	"prune": {"project", 90}, "git": {"project", 12}, "check": {"project", 14}, "review": {"project", 100},
	"context": {"coordinate", 10}, "message": {"coordinate", 20}, "approval": {"coordinate", 30},
	"ctx": {"coordinate", 40}, "msg": {"coordinate", 50}, "approvals": {"coordinate", 60}, "approve": {"coordinate", 70},
	"auto-approve": {"coordinate", 80}, "collab": {"coordinate", 90},
	"search": {"observe", 10}, "history": {"observe", 20}, "audit": {"observe", 30}, "stats": {"observe", 40},
	"inspect": {"observe", 35}, "usage": {"observe", 50}, "cost": {"observe", 60}, "spend": {"observe", 70}, "savings": {"observe", 80},
	"insights": {"observe", 90}, "backend": {"observe", 45}, "backends": {"observe", 100}, "models": {"observe", 110}, "config": {"observe", 120},
	"export": {"observe", 130}, "import": {"observe", 140}, "llm": {"observe", 150}, "repair": {"observe", 160},
	"daemon": {"operate", 10}, "completion": {"operate", 20}, "mcp": {"operate", 30}, "token": {"operate", 40},
	"setup": {"entry", 10}, "tutorial": {"entry", 20}, "doctor": {"entry", 30}, "tui": {"entry", 40}, "repl": {"entry", 50}, "version": {"entry", 60},
}

// SetCommandHelpMetadata is the extension point used by namespace factories.
// Values already supplied by a factory win over inferred foundation defaults.
func SetCommandHelpMetadata(cmd *cobra.Command, group string, order int, canonicalPath, aliasKind, nodeKind string) {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	setAnnotationDefault(cmd, AnnotationHelpGroup, group)
	if order > 0 {
		setAnnotationDefault(cmd, AnnotationHelpOrder, strconv.Itoa(order))
	}
	setAnnotationDefault(cmd, AnnotationCanonicalPath, canonicalPath)
	setAnnotationDefault(cmd, AnnotationAliasKind, aliasKind)
	setAnnotationDefault(cmd, AnnotationNodeKind, nodeKind)
	setAnnotationDefault(cmd, AnnotationIncludeInAll, "true")
	setAnnotationDefault(cmd, AnnotationIncludeInDocs, "true")
	setAnnotationDefault(cmd, AnnotationIncludeComplete, "true")
}

func setAnnotationDefault(cmd *cobra.Command, key, value string) {
	if value != "" && cmd.Annotations[key] == "" {
		cmd.Annotations[key] = value
	}
}

func installCommandHelp(root *cobra.Command) error {
	annotateExistingTree(root)
	if err := ValidateCommandTree(root); err != nil {
		return err
	}
	root.SetHelpFunc(func(cmd *cobra.Command, _ []string) { _ = renderCommandHelp(cmd.OutOrStdout(), cmd, false) })
	root.SetHelpCommand(newHelpTraversalCmd(root))
	return nil
}

func annotateExistingTree(root *cobra.Command) {
	commands := append([]*cobra.Command(nil), root.Commands()...)
	sort.SliceStable(commands, func(i, j int) bool { return commands[i].Name() < commands[j].Name() })
	for i, cmd := range commands {
		placement, ok := rootHelpPlacement[cmd.Name()]
		if !ok {
			placement = struct {
				group string
				order int
			}{"run", 1000 + i}
		}
		kind := NodeLeaf
		if cmd.HasAvailableSubCommands() {
			kind = NodeNamespace
		}
		if cmd.Name() == "hook" {
			kind = NodeInternal
		}
		SetCommandHelpMetadata(cmd, placement.group, placement.order, cmd.CommandPath(), "", kind)
		annotateChildren(cmd, placement.group)
	}
}

func annotateChildren(parent *cobra.Command, group string) {
	children := append([]*cobra.Command(nil), parent.Commands()...)
	sort.SliceStable(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
	for i, cmd := range children {
		kind := NodeLeaf
		if cmd.HasAvailableSubCommands() {
			kind = NodeNamespace
		}
		SetCommandHelpMetadata(cmd, group, (i+1)*10, cmd.CommandPath(), "", kind)
		annotateChildren(cmd, group)
	}
}

// ValidateCommandTree catches metadata mistakes before a command tree is used.
func ValidateCommandTree(root *cobra.Command) error {
	known := map[string]bool{}
	for _, g := range helpGroups {
		known[g.ID] = true
	}
	var errs []string
	var walk func(*cobra.Command)
	walk = func(parent *cobra.Command) {
		seen := map[string]string{}
		for _, cmd := range parent.Commands() {
			if cmd.Name() == "help" {
				continue
			}
			group, order := cmd.Annotations[AnnotationHelpGroup], cmd.Annotations[AnnotationHelpOrder]
			if !known[group] {
				errs = append(errs, fmt.Sprintf("%s: unknown help group %q", cmd.CommandPath(), group))
			}
			if _, err := strconv.Atoi(order); err != nil {
				errs = append(errs, fmt.Sprintf("%s: invalid help order %q", cmd.CommandPath(), order))
			}
			if !cmd.Hidden {
				key := group + "/" + order
				if other := seen[key]; other != "" {
					errs = append(errs, fmt.Sprintf("%s and %s: duplicate visible help order", other, cmd.CommandPath()))
				}
				seen[key] = cmd.CommandPath()
			}
			if cmd.Annotations[AnnotationAliasKind] == AliasCompatibility && cmd.Annotations[AnnotationCanonicalPath] == "" {
				errs = append(errs, fmt.Sprintf("%s: compatibility alias has no canonical path", cmd.CommandPath()))
			}
			walk(cmd)
		}
	}
	walk(root)
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("invalid command help metadata:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

func newHelpTraversalCmd(root *cobra.Command) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use: "help [command path...]", Short: "Explore command help", Args: cobra.ArbitraryArgs,
		PersistentPreRun: func(*cobra.Command, []string) {},
		RunE: func(cmd *cobra.Command, args []string) error {
			target, remaining, err := root.Find(args)
			if err != nil || len(remaining) != 0 {
				path := strings.Join(args, " ")
				return fmt.Errorf("unknown help path %q; run 'warden help' to browse commands", path)
			}
			return renderCommandHelp(cmd.OutOrStdout(), target, all)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "show the complete command tree and compatibility aliases")
	return cmd
}

func renderCommandHelp(w io.Writer, cmd *cobra.Command, all bool) error {
	cmd.InitDefaultHelpFlag()
	cmd.InitDefaultVersionFlag()
	if all {
		return renderAllHelp(w, cmd.Root())
	}
	if cmd == cmd.Root() {
		return renderRootHelp(w, cmd)
	}
	return renderFocusedHelp(w, cmd)
}

func renderRootHelp(w io.Writer, root *cobra.Command) error {
	fmt.Fprintln(w, root.Long)
	fmt.Fprintf(w, "\nUsage:\n  %s [command]\n  %s [flags]\n", root.CommandPath(), root.CommandPath())
	for _, group := range helpGroups {
		commands := groupedChildren(root, group.ID)
		if len(commands) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s:\n", group.Title)
		for _, cmd := range commands {
			fmt.Fprintf(w, "  %-20s %s\n", cmd.Name(), cmd.Short)
		}
	}
	fmt.Fprintln(w, "\nUse \"warden help <command>\" for focused command help; add --all for the complete tree.")
	return nil
}

func groupedChildren(parent *cobra.Command, group string) []*cobra.Command {
	var out []*cobra.Command
	for _, cmd := range parent.Commands() {
		if cmd.Hidden || cmd.Name() == "help" || cmd.Annotations[AnnotationHelpGroup] != group {
			continue
		}
		out = append(out, cmd)
	}
	sortCommands(out)
	return out
}

func sortCommands(commands []*cobra.Command) {
	sort.SliceStable(commands, func(i, j int) bool {
		groupOrder := func(cmd *cobra.Command) int {
			for _, group := range helpGroups {
				if group.ID == cmd.Annotations[AnnotationHelpGroup] {
					return group.Order
				}
			}
			return 1000
		}
		ga, gb := groupOrder(commands[i]), groupOrder(commands[j])
		if ga != gb {
			return ga < gb
		}
		a, _ := strconv.Atoi(commands[i].Annotations[AnnotationHelpOrder])
		b, _ := strconv.Atoi(commands[j].Annotations[AnnotationHelpOrder])
		if a != b {
			return a < b
		}
		return commands[i].Name() < commands[j].Name()
	})
}

func renderFocusedHelp(w io.Writer, cmd *cobra.Command) error {
	description := cmd.Long
	if description == "" {
		description = cmd.Short
	}
	if description != "" {
		fmt.Fprintln(w, description)
	}
	fmt.Fprintf(w, "\nUsage:\n  %s\n", cmd.UseLine())
	children := make([]*cobra.Command, 0)
	for _, child := range cmd.Commands() {
		if !child.Hidden && child.Name() != "help" {
			children = append(children, child)
		}
	}
	if len(children) > 0 {
		sortCommands(children)
		fmt.Fprintln(w, "\nCommands:")
		for _, child := range children {
			fmt.Fprintf(w, "  %-20s %s\n", child.Name(), child.Short)
		}
	}
	if cmd.Example != "" {
		fmt.Fprintf(w, "\nExamples:\n%s\n", indent(cmd.Example, "  "))
	}
	renderFlags(w, "Flags", cmd.LocalNonPersistentFlags())
	renderFlags(w, "Inherited flags", cmd.InheritedFlags())
	if len(cmd.Aliases) > 0 {
		fmt.Fprintf(w, "\nAliases:\n  %s\n", strings.Join(cmd.Aliases, ", "))
	}
	return nil
}

func renderFlags(w io.Writer, title string, flags *pflag.FlagSet) {
	if flags == nil || !flags.HasAvailableFlags() {
		return
	}
	fmt.Fprintf(w, "\n%s:\n", title)
	flags.SetOutput(w)
	flags.PrintDefaults()
}

func indent(s, prefix string) string {
	return prefix + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n"+prefix)
}

// CompatibilityAlias is one legacy path that remains executable, paired with the
// canonical path that replaced it. Canonical equals Path for a legacy command that
// was deliberately retained without a canonical successor.
type CompatibilityAlias struct {
	Path      string
	Canonical string
}

// Retained reports whether this legacy path has no canonical replacement.
func (a CompatibilityAlias) Retained() bool { return a.Path == a.Canonical }

// WalkCommandTree visits every non-help node that opts into `--all` traversal, in
// deterministic canonical order. It is the single traversal behind `help --all`,
// the generated CLI reference, and the alias appendix.
func WalkCommandTree(root *cobra.Command, visit func(*cobra.Command)) {
	var walk func(*cobra.Command)
	walk = func(parent *cobra.Command) {
		children := append([]*cobra.Command(nil), parent.Commands()...)
		sortCommands(children)
		for _, cmd := range children {
			if cmd.Name() == "help" || cmd.Annotations[AnnotationIncludeInAll] == "false" {
				continue
			}
			visit(cmd)
			walk(cmd)
		}
	}
	walk(root)
}

func canonicalPathOf(cmd *cobra.Command) string {
	if canonical := cmd.Annotations[AnnotationCanonicalPath]; canonical != "" {
		return canonical
	}
	return cmd.CommandPath()
}

// CollectCompatibilityAliases returns every inventoried legacy path in the tree,
// sorted by legacy path.
func CollectCompatibilityAliases(root *cobra.Command) []CompatibilityAlias {
	var aliases []CompatibilityAlias
	WalkCommandTree(root, func(cmd *cobra.Command) {
		canonical := canonicalPathOf(cmd)
		if cmd.Annotations[AnnotationAliasKind] == AliasCompatibility {
			aliases = append(aliases, CompatibilityAlias{cmd.CommandPath(), canonical})
		}
		for _, alias := range cmd.Aliases {
			aliases = append(aliases, CompatibilityAlias{cmd.Parent().CommandPath() + " " + alias, canonical})
		}
	})
	sort.Slice(aliases, func(i, j int) bool { return aliases[i].Path < aliases[j].Path })
	return aliases
}

func formatAliasEntry(path, canonical string) string {
	if path == canonical {
		return fmt.Sprintf("%s (compatibility; no canonical equivalent)", path)
	}
	return fmt.Sprintf("%s -> %s (compatibility)", path, canonical)
}

func renderAllHelp(w io.Writer, root *cobra.Command) error {
	fmt.Fprintln(w, "Complete command tree:")
	WalkCommandTree(root, func(cmd *cobra.Command) {
		if cmd.Annotations[AnnotationAliasKind] == AliasCompatibility {
			return
		}
		if cmd.Annotations[AnnotationNodeKind] == NodeInternal {
			return
		}
		fmt.Fprintf(w, "  %-36s %s\n", cmd.CommandPath(), cmd.Short)
	})
	aliases := CollectCompatibilityAliases(root)
	if len(aliases) > 0 {
		fmt.Fprintln(w, "\nCompatibility aliases:")
		for _, alias := range aliases {
			fmt.Fprintln(w, "  "+formatAliasEntry(alias.Path, alias.Canonical))
		}
	}
	return nil
}
