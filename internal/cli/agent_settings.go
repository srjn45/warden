package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/role"
)

var permissionModeChoices = []string{"acceptEdits", "auto", "bypassPermissions", "default", "dontAsk", "plan"}

// agentSetting is one per-agent setting reachable through `agent set` / `agent get`.
// run delegates to the legacy command's RunE so both paths share one client call.
type agentSetting struct {
	key     string
	values  func() []string
	newCmd  func() *cobra.Command // legacy command whose RunE does the work
	display func(s settingSnapshot) string
}

type settingSnapshot struct {
	PermissionMode string
	Compact        string
	Role           string
	AutoApprove    string
}

var agentSettings = []agentSetting{
	{"permission-mode", func() []string { return permissionModeChoices }, newSetPermissionModeCmd,
		func(s settingSnapshot) string { return s.PermissionMode }},
	{"compact", func() []string { return []string{"on", "off", "inherit"} }, newForceCompactCmd,
		func(s settingSnapshot) string { return s.Compact }},
	{"role", func() []string { return role.Names() }, newSetRoleCmd,
		func(s settingSnapshot) string { return s.Role }},
	{"auto-approve", func() []string { return []string{"on", "off"} }, newAutoApproveSetCmd,
		func(s settingSnapshot) string { return s.AutoApprove }},
}

func agentSettingKeys() []string {
	keys := make([]string, 0, len(agentSettings))
	for _, s := range agentSettings {
		keys = append(keys, s.key)
	}
	return keys
}

func findAgentSetting(key string) (agentSetting, error) {
	for _, s := range agentSettings {
		if s.key == key {
			return s, nil
		}
	}
	return agentSetting{}, fmt.Errorf("unknown setting %q (valid: %s)", key, strings.Join(agentSettingKeys(), ", "))
}

func newAgentSetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <AGENT> <key> <value>",
		Short: "Change one per-agent setting (permission-mode, compact, role, auto-approve)",
		Long: `Change one per-agent setting. Each key runs the same code path as its older command.

Keys and values:
  permission-mode  ` + strings.Join(permissionModeChoices, " | ") + `
  compact          on | off | inherit
  role             ` + strings.Join(role.Names(), " | ") + `
  auto-approve     on | off

Warnings:
  role          RELAUNCHES the agent so the new persona is injected; its current
                turn is discarded.
  compact on    force-compact can INTERRUPT an in-flight turn (Escape, /compact,
                then resume) when the context crosses the critical threshold.

Examples:
  warden agent set abc123 permission-mode acceptEdits
  warden agent set abc123 compact inherit
  warden agent set abc123 role reviewer
  warden agent set abc123 auto-approve on

See also: 'warden agent get <AGENT> [key]' to read the current values.`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := findAgentSetting(args[1])
			if err != nil {
				return err
			}
			legacy := s.newCmd()
			return legacy.RunE(cmd, []string{args[0], args[2]})
		},
		ValidArgsFunction: func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			switch len(args) {
			case 1:
				return agentSettingKeys(), cobra.ShellCompDirectiveNoFileComp
			case 2:
				if s, err := findAgentSetting(args[1]); err == nil {
					return s.values(), cobra.ShellCompDirectiveNoFileComp
				}
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
	}
	return cmd
}

func newAgentGetCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "get <AGENT> [key]",
		Short: "Show one or all per-agent settings (permission-mode, compact, role, auto-approve)",
		Long: `Show the current value of one or all per-agent settings, read from the agent's
status record.

Keys: ` + strings.Join(agentSettingKeys(), ", ") + `

An empty permission-mode means no per-agent override (the global default applies);
compact "inherit" means the global token_force_compact setting applies; role
"general" means no persona.

Examples:
  warden agent get abc123
  warden agent get abc123 role
  warden agent get abc123 --json`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var want *agentSetting
			if len(args) == 2 {
				s, err := findAgentSetting(args[1])
				if err != nil {
					return err
				}
				want = &s
			}
			sess, err := clientFor(cmd).Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			snap := settingSnapshot{
				PermissionMode: sess.PermissionMode,
				Compact:        "inherit",
				Role:           sess.Role,
				AutoApprove:    "off",
			}
			if snap.Role == "" {
				snap.Role = "general"
			}
			if sess.ForceCompact != nil {
				snap.Compact = map[bool]string{true: "on", false: "off"}[*sess.ForceCompact]
			}
			if sess.AutoApprove {
				snap.AutoApprove = "on"
			}
			out := cmd.OutOrStdout()
			if want != nil {
				if asJSON {
					return writeJSON(out, map[string]string{want.key: want.display(snap)})
				}
				fmt.Fprintln(out, want.display(snap))
				return nil
			}
			if asJSON {
				m := map[string]string{}
				for _, s := range agentSettings {
					m[s.key] = s.display(snap)
				}
				return writeJSON(out, m)
			}
			w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "SETTING\tVALUE")
			for _, s := range agentSettings {
				fmt.Fprintf(w, "%s\t%s\n", s.key, s.display(snap))
			}
			return w.Flush()
		},
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 1 {
				return agentSettingKeys(), cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit settings as JSON")
	return cmd
}

func writeJSON(w interface{ Write([]byte) (int, error) }, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
