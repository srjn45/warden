package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/approval"
	"gopkg.in/yaml.v3"
)

func newAutoApproveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auto-approve <agent-id> <on|off>",
		Short: "Toggle per-agent auto-approve, or manage the auto-approve rule policy",
		Long: `Control automatic approval of recognized tool-permission prompts.

Two layers cooperate:

  • Per-agent toggle (this command's bare form): opt one agent into evaluation
    even when the global policy is disabled.
        warden auto-approve abc123 on    # participate
        warden auto-approve abc123 off   # stop

  • Policy rules (the subcommands below): an allow/deny rule engine evaluated for
    every participating agent. A prompt is auto-answered only when it matches an
    allow rule, matches NO deny rule, and is not on warden's built-in destructive
    deny-list (which always wins). With NO rules configured the policy is the
    simple on/off toggle: an enabled policy approves every recognized,
    non-destructive prompt (backward compatible with the old behavior).

Rules match by tool name, a case-insensitive glob/substring (--pattern), a Go
regular expression (--regex) over the prompt, and/or path globs (--paths). A
per-agent override (--agent NAME, keyed by agent name or id) gets its own rule
set that replaces the default for that agent.

Examples:
  warden auto-approve rules                         # show the live policy
  warden auto-approve enable                        # turn the policy on
  warden auto-approve allow --tool Read             # auto-approve all Read prompts
  warden auto-approve allow --regex '^Bash\(git (status|diff|log)\)'
  warden auto-approve deny  --tool Bash --pattern rm
  warden auto-approve allow --agent reviewer --tool Grep
  warden auto-approve clear --agent reviewer        # drop reviewer's overrides

Rule changes take effect immediately (no restart) and are persisted to config.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			mode := args[1]

			var enabled bool
			switch mode {
			case "on", "1", "true":
				enabled = true
			case "off", "0", "false":
				enabled = false
			default:
				return fmt.Errorf("mode must be 'on' or 'off', got %q", mode)
			}

			c := clientFor(cmd)
			if err := c.SetAutoApprove(cmd.Context(), id, enabled); err != nil {
				return err
			}

			status := "disabled"
			if enabled {
				status = "enabled"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "auto-approve %s for %s\n", status, id)
			return nil
		},
	}
	cmd.AddCommand(
		newAutoApproveRulesCmd(),
		newAutoApproveAddRuleCmd("allow"),
		newAutoApproveAddRuleCmd("deny"),
		newAutoApproveClearCmd(),
		newAutoApproveToggleCmd("enable", "Enable the auto-approve policy (global, or per-agent with --agent)", true),
		newAutoApproveToggleCmd("disable", "Disable the auto-approve policy (global, or per-agent with --agent)", false),
	)
	return cmd
}

func newAutoApproveRulesCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rules",
		Aliases: []string{"show", "policy"},
		Short:   "Show the live auto-approve policy (default rules + per-agent overrides)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pol, err := clientFor(cmd).GetAutoApprovePolicy(cmd.Context())
			if err != nil {
				return err
			}
			out, err := yaml.Marshal(pol)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), string(out))
			return nil
		},
	}
}

// newAutoApproveAddRuleCmd builds the `allow` / `deny` subcommands, which append
// a rule to the default (or a per-agent) rule list.
func newAutoApproveAddRuleCmd(kind string) *cobra.Command {
	var agent, tool, pattern, regex string
	var paths []string
	cmd := &cobra.Command{
		Use:   kind,
		Short: "Append an " + kind + " rule to the auto-approve policy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rule := approval.Rule{Tool: tool, Pattern: pattern, Regex: regex, Paths: paths}
			if tool == "" && pattern == "" && regex == "" && len(paths) == 0 {
				// An empty rule matches everything — refuse it as a silent foot-gun.
				return fmt.Errorf("refusing to add an empty %s rule (matches everything); set at least one of --tool/--pattern/--regex/--paths", kind)
			}
			if regex != "" {
				if err := (approval.Policy{Rules: approval.Rules{Allow: []approval.Rule{rule}}}).Validate(); err != nil {
					return err
				}
			}
			c := clientFor(cmd)
			pol, err := c.GetAutoApprovePolicy(cmd.Context())
			if err != nil {
				return err
			}
			mutate := func(rules *approval.Rules) {
				if kind == "allow" {
					rules.Allow = append(rules.Allow, rule)
				} else {
					rules.Deny = append(rules.Deny, rule)
				}
			}
			applyToAgent(&pol, agent, func(p *approval.Policy) { mutate(&p.Rules) })
			saved, err := c.PutAutoApprovePolicy(cmd.Context(), pol)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added %s rule%s: %s\n", kind, agentSuffix(agent), describeRule(rule))
			return printRuleCounts(cmd, saved, agent)
		},
	}
	f := cmd.Flags()
	f.StringVar(&agent, "agent", "", "scope the rule to a per-agent override (agent name or id)")
	f.StringVar(&tool, "tool", "", "exact tool name to match (e.g. Read, Bash)")
	f.StringVar(&pattern, "pattern", "", "case-insensitive glob/substring over the action + question")
	f.StringVar(&regex, "regex", "", "Go regular expression over the action + question")
	f.StringSliceVar(&paths, "paths", nil, "path globs against the action target (comma-separated)")
	return cmd
}

func newAutoApproveClearCmd() *cobra.Command {
	var agent string
	cmd := &cobra.Command{
		Use:   "clear",
		Short: "Clear auto-approve rules (default, or a per-agent override with --agent)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := clientFor(cmd)
			pol, err := c.GetAutoApprovePolicy(cmd.Context())
			if err != nil {
				return err
			}
			if agent != "" {
				delete(pol.Agents, agent)
			} else {
				pol.Rules = approval.Rules{}
			}
			if _, err := c.PutAutoApprovePolicy(cmd.Context(), pol); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "cleared auto-approve rules%s\n", agentSuffix(agent))
			return nil
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "clear a per-agent override (agent name or id) instead of the default rules")
	return cmd
}

// newAutoApproveToggleCmd builds `enable` / `disable`, which flip the policy's
// master switch (or a per-agent override's switch with --agent).
func newAutoApproveToggleCmd(use, short string, on bool) *cobra.Command {
	var agent string
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := clientFor(cmd)
			pol, err := c.GetAutoApprovePolicy(cmd.Context())
			if err != nil {
				return err
			}
			applyToAgent(&pol, agent, func(p *approval.Policy) { p.Enabled = on })
			if _, err := c.PutAutoApprovePolicy(cmd.Context(), pol); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "auto-approve policy %sd%s\n", use, agentSuffix(agent))
			return nil
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "toggle a per-agent override (agent name or id) instead of the global default")
	return cmd
}

// applyToAgent applies fn to the default policy (agent == "") or to the named
// per-agent override, creating the override (and Agents map) on first use.
func applyToAgent(pol *approval.Policy, agent string, fn func(*approval.Policy)) {
	if agent == "" {
		fn(pol)
		return
	}
	if pol.Agents == nil {
		pol.Agents = map[string]approval.Policy{}
	}
	ov := pol.Agents[agent]
	fn(&ov)
	pol.Agents[agent] = ov
}

func agentSuffix(agent string) string {
	if agent == "" {
		return ""
	}
	return " for agent " + agent
}

func describeRule(r approval.Rule) string {
	var parts []string
	if r.Tool != "" {
		parts = append(parts, "tool="+r.Tool)
	}
	if r.Pattern != "" {
		parts = append(parts, "pattern="+r.Pattern)
	}
	if r.Regex != "" {
		parts = append(parts, "regex="+r.Regex)
	}
	if len(r.Paths) > 0 {
		parts = append(parts, "paths="+strings.Join(r.Paths, ","))
	}
	if len(parts) == 0 {
		return "(matches everything)"
	}
	return strings.Join(parts, " ")
}

func printRuleCounts(cmd *cobra.Command, pol approval.Policy, agent string) error {
	rules := pol.Rules
	if agent != "" {
		rules = pol.Agents[agent].Rules
	}
	fmt.Fprintf(cmd.OutOrStdout(), "policy now has %d allow / %d deny rule(s)%s\n", len(rules.Allow), len(rules.Deny), agentSuffix(agent))
	return nil
}
