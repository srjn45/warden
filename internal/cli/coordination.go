package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

// newContextCmd builds the canonical context namespace. Each child is allocated
// by the same fresh factory used by its legacy ctx wrapper.
func newContextCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "Read and write the shared context (a namespaced key/value store agents share)",
	}
	SetCommandHelpMetadata(cmd, "coordinate", 10, "warden context", "", NodeNamespace)

	children := []*cobra.Command{
		canonicalContextCommand(newCtxSetCmd(), "set"),
		canonicalContextCommand(newCtxCASCmd(), "cas"),
		canonicalContextCommand(newCtxAppendCmd(), "append"),
		canonicalContextCommand(newCtxGetCmd(), "get"),
		canonicalContextCommand(newCtxListCmd(), "list"),
		canonicalContextCommand(newCtxDelCmd(), "delete"),
	}
	for i, child := range children {
		SetCommandHelpMetadata(child, "coordinate", (i+1)*10, "warden context "+child.Name(), "", nodeKind(child))
		cmd.AddCommand(child)
	}
	return cmd
}

func canonicalContextCommand(cmd *cobra.Command, name string) *cobra.Command {
	parts := strings.SplitN(cmd.Use, " ", 2)
	legacyName := parts[0]
	rewriteContextHelpPaths(cmd, legacyName, name)
	cmd.Use = name
	if len(parts) == 2 {
		cmd.Use += " " + parts[1]
	}
	cmd.Aliases = nil
	return cmd
}

func rewriteContextHelpPaths(cmd *cobra.Command, legacyName, canonicalName string) {
	replacer := strings.NewReplacer(
		"warden ctx "+legacyName, "warden context "+canonicalName,
		"wd ctx "+legacyName, "wd context "+canonicalName,
		"warden ctx", "warden context",
		"wd ctx", "wd context",
	)
	cmd.Long = replacer.Replace(cmd.Long)
	cmd.Example = replacer.Replace(cmd.Example)
}

// newMessageCmd builds the canonical message namespace. Directed mailbox traffic
// stays distinct from terminal agent send.
func newMessageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "message",
		Short: "Send and receive directed messages between agents",
	}
	cmd.PersistentFlags().String("as", "", "act as this agent id (defaults to $WARDEN_SESSION_ID)")
	SetCommandHelpMetadata(cmd, "coordinate", 20, "warden message", "", NodeNamespace)

	children := []*cobra.Command{
		canonicalMessageCommand(newMsgSendCmd(), "send"),
		canonicalMessageCommand(newMsgInboxCmd(), "inbox"),
		canonicalMessageCommand(newMsgWaitCmd(), "wait"),
	}
	for i, child := range children {
		SetCommandHelpMetadata(child, "coordinate", (i+1)*10, "warden message "+child.Name(), "", nodeKind(child))
		cmd.AddCommand(child)
	}
	return cmd
}

func canonicalMessageCommand(cmd *cobra.Command, name string) *cobra.Command {
	parts := strings.SplitN(cmd.Use, " ", 2)
	legacyName := parts[0]
	rewriteMessageHelpPaths(cmd, legacyName, name)
	cmd.Use = name
	if len(parts) == 2 {
		cmd.Use += " " + parts[1]
	}
	cmd.Aliases = nil
	return cmd
}

func rewriteMessageHelpPaths(cmd *cobra.Command, legacyName, canonicalName string) {
	replacer := strings.NewReplacer(
		"warden msg "+legacyName, "warden message "+canonicalName,
		"wd msg "+legacyName, "wd message "+canonicalName,
		"warden msg", "warden message",
		"wd msg", "wd message",
	)
	cmd.Long = replacer.Replace(cmd.Long)
	cmd.Example = replacer.Replace(cmd.Example)
}

// newApprovalCmd builds the canonical approval namespace for pending prompts and
// auto-approve policy management.
func newApprovalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approval",
		Short: "Manage tool-permission prompts and auto-approve policy",
		Long: `Manage tool-permission prompts and the auto-approve policy.

List pending prompts with list, answer recognized menus by option number with
answer, and configure per-agent participation plus allow/deny rules under auto.`,
	}
	SetCommandHelpMetadata(cmd, "coordinate", 30, "warden approval", "", NodeNamespace)

	children := []*cobra.Command{
		canonicalApprovalCommand(newApprovalsCmd(), "list", "approvals"),
		canonicalApprovalCommand(newApproveCmd(), "answer", "approve"),
		newApprovalAutoCmd(),
	}
	for i, child := range children {
		SetCommandHelpMetadata(child, "coordinate", (i+1)*10, "warden approval "+child.Name(), "", nodeKind(child))
		cmd.AddCommand(child)
	}
	return cmd
}

func canonicalApprovalCommand(cmd *cobra.Command, name, legacyName string) *cobra.Command {
	parts := strings.SplitN(cmd.Use, " ", 2)
	if legacyName == "" {
		legacyName = parts[0]
	}
	rewriteApprovalHelpPaths(cmd, legacyName, name)
	cmd.Use = name
	if len(parts) == 2 {
		cmd.Use += " " + parts[1]
	}
	cmd.Aliases = nil
	return cmd
}

func rewriteApprovalHelpPaths(cmd *cobra.Command, legacyName, canonicalName string) {
	replacer := strings.NewReplacer(
		"warden "+legacyName, "warden approval "+canonicalName,
		"wd "+legacyName, "wd approval "+canonicalName,
		"warden auto-approve", "warden approval auto",
		"wd auto-approve", "wd approval auto",
	)
	cmd.Long = replacer.Replace(cmd.Long)
	cmd.Example = replacer.Replace(cmd.Example)
}

func newApprovalAutoCmd() *cobra.Command {
	long := newAutoApproveCmd().Long
	cmd := &cobra.Command{
		Use:   "auto",
		Short: "Toggle per-agent auto-approve and manage the rule policy",
		Long:  rewriteApprovalHelpPathsText(long),
	}
	SetCommandHelpMetadata(cmd, "coordinate", 30, "warden approval auto", "", NodeNamespace)

	children := []*cobra.Command{
		canonicalApprovalAutoCommand(newAutoApproveSetCmd(), "set"),
		canonicalApprovalAutoCommand(newAutoApproveRulesCmd(), "rules"),
		canonicalApprovalAutoCommand(newAutoApproveAddRuleCmd("allow"), "allow"),
		canonicalApprovalAutoCommand(newAutoApproveAddRuleCmd("deny"), "deny"),
		canonicalApprovalAutoCommand(newAutoApproveClearCmd(), "clear"),
		canonicalApprovalAutoCommand(newAutoApproveToggleCmd("enable", "Enable the auto-approve policy (global, or per-agent with --agent)", true), "enable"),
		canonicalApprovalAutoCommand(newAutoApproveToggleCmd("disable", "Disable the auto-approve policy (global, or per-agent with --agent)", false), "disable"),
	}
	for i, child := range children {
		SetCommandHelpMetadata(child, "coordinate", (i+1)*10, "warden approval auto "+child.Name(), "", nodeKind(child))
		cmd.AddCommand(child)
	}
	return cmd
}

func canonicalApprovalAutoCommand(cmd *cobra.Command, name string) *cobra.Command {
	parts := strings.SplitN(cmd.Use, " ", 2)
	legacyName := parts[0]
	rewriteApprovalHelpPaths(cmd, "auto-approve "+legacyName, "auto "+name)
	if legacyName == "auto-approve" {
		rewriteApprovalHelpPaths(cmd, legacyName, "auto "+name)
	}
	cmd.Use = name
	if len(parts) == 2 {
		cmd.Use += " " + parts[1]
	}
	cmd.Aliases = nil
	return cmd
}

func rewriteApprovalHelpPathsText(s string) string {
	replacer := strings.NewReplacer(
		"warden auto-approve", "warden approval auto",
		"wd auto-approve", "wd approval auto",
	)
	return replacer.Replace(s)
}

func markCtxCompatibility(cmd *cobra.Command) {
	cmd.Hidden = true
	SetCommandHelpMetadata(cmd, "coordinate", 900, "warden context", AliasCompatibility, NodeNamespace)
	for _, child := range cmd.Commands() {
		canonicalChild := child.Name()
		if canonicalChild == "del" {
			canonicalChild = "delete"
		}
		markCompatibilityChild(child, "warden context "+canonicalChild)
	}
}

func markMsgCompatibility(cmd *cobra.Command) {
	markCompatibilityCommand(cmd, "warden message")
}

func markApprovalsCompatibility(cmd *cobra.Command) {
	cmd.Hidden = true
	SetCommandHelpMetadata(cmd, "coordinate", 900, "warden approval list", AliasCompatibility, NodeLeaf)
}

func markApproveCompatibility(cmd *cobra.Command) {
	cmd.Hidden = true
	SetCommandHelpMetadata(cmd, "coordinate", 900, "warden approval answer", AliasCompatibility, NodeLeaf)
}

func markAutoApproveCompatibility(cmd *cobra.Command) {
	cmd.Hidden = true
	SetCommandHelpMetadata(cmd, "coordinate", 900, "warden approval auto set", AliasCompatibility, NodeNamespace)
	for _, child := range cmd.Commands() {
		markCompatibilityChild(child, "warden approval auto "+child.Name())
	}
}
