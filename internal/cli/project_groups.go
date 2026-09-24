package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/projectstore"
)

// newProjectGroupsCmd builds the canonical project-groups namespace
// (docs/specs/2026-09-05-project-groups-membership-and-cli.md).
func newProjectGroupsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project-groups",
		Short: "Inspect and manage project groups and membership",
		Long: "Inspect and manage project groups and their member projects.\n\n" +
			"A project group is a named collection of projects displayed together in the\n" +
			"cockpit TUI. Member projects carry the group label beside their name, and\n" +
			"orchestrators in the same group gain peer awareness of each other.",
	}
	SetCommandHelpMetadata(cmd, "project", 7, "warden project-groups", "", NodeNamespace)

	membersCmd := newProjectGroupsMembersCmd()

	children := []*cobra.Command{
		newProjectGroupsListCmd(),
		newProjectGroupsShowCmd(),
		newProjectGroupsCreateCmd(),
		newProjectGroupsUpdateCmd(),
		newProjectGroupsDeleteCmd(),
		membersCmd,
	}
	for i, child := range children {
		SetCommandHelpMetadata(child, "project", (i+1)*10, "warden project-groups "+child.Name(), "", nodeKind(child))
		cmd.AddCommand(child)
	}
	return cmd
}

func newProjectGroupsListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List project groups",
		Long:    "List all project groups with their member projects.",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			groups, err := clientFor(cmd).ListProjectGroups(cmd.Context())
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				if groups == nil {
					groups = []projectstore.ProjectGroup{}
				}
				return printJSON(cmd.OutOrStdout(), groups)
			}
			if len(groups) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no project groups found")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tMEMBERS")
			for _, g := range groups {
				members := "(none)"
				if len(g.ProjectIDs) > 0 {
					members = strings.Join(g.ProjectIDs, ", ")
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", g.ID, g.Name, members)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func newProjectGroupsShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show details for a project group",
		Long:  "Show detailed information about a project group, including its member projects.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			g, err := clientFor(cmd).GetProjectGroup(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), g)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "ID:      %s\n", g.ID)
			fmt.Fprintf(out, "Name:    %s\n", g.Name)
			if len(g.ProjectIDs) == 0 {
				fmt.Fprintln(out, "Members: (none)")
			} else {
				fmt.Fprintf(out, "Members (%d):\n", len(g.ProjectIDs))
				for _, p := range g.ProjectIDs {
					fmt.Fprintf(out, "  - %s\n", p)
				}
			}
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func newProjectGroupsCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new project group",
		Long:  "Create a new named project group with optional initial member projects.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, _ := cmd.Flags().GetStringSlice("project")
			g, err := clientFor(cmd).CreateProjectGroup(cmd.Context(), args[0], projects)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created project group %s (%s)\n", g.ID, g.Name)
			return nil
		},
	}
	cmd.Flags().StringSliceP("project", "p", nil, "initial member project ID (repeatable)")
	return cmd
}

func newProjectGroupsUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update <id>",
		Short: "Update a project group's name and membership (bulk-set)",
		Long:  "Update a project group's display name and/or overwrite its member project list.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			c := clientFor(cmd)
			nameFlagChanged := cmd.Flags().Changed("name")
			projectFlagChanged := cmd.Flags().Changed("project")

			name, _ := cmd.Flags().GetString("name")
			projects, _ := cmd.Flags().GetStringSlice("project")

			if !nameFlagChanged || !projectFlagChanged {
				existing, err := c.GetProjectGroup(cmd.Context(), id)
				if err != nil {
					return err
				}
				if !nameFlagChanged {
					name = existing.Name
				}
				if !projectFlagChanged {
					projects = existing.ProjectIDs
				}
			}
			g, err := c.UpdateProjectGroup(cmd.Context(), id, name, projects)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "updated project group %s (%s)\n", g.ID, g.Name)
			return nil
		},
	}
	cmd.Flags().StringP("name", "n", "", "new display name")
	cmd.Flags().StringSliceP("project", "p", nil, "member project IDs to set (repeatable; overwrites full membership)")
	return cmd
}

func newProjectGroupsDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a project group",
		Long:  "Delete a project group. Member projects remain registered and are not touched.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := clientFor(cmd).DeleteProjectGroup(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted project group %s\n", args[0])
			return nil
		},
	}
	return cmd
}

func newProjectGroupsMembersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "members",
		Short: "Manage project group members",
		Long:  "Add or remove member projects in a project group incrementally.",
	}
	children := []*cobra.Command{
		newProjectGroupsMembersAddCmd(),
		newProjectGroupsMembersRemoveCmd(),
	}
	for i, child := range children {
		SetCommandHelpMetadata(child, "project", (i+1)*10, "warden project-groups members "+child.Name(), "", nodeKind(child))
		cmd.AddCommand(child)
	}
	return cmd
}

func newProjectGroupsMembersAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <group-id> <project-id>",
		Short: "Add a project to a project group",
		Long:  "Incrementally add a project (canonical ID, path, or URL) to a project group.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			g, err := clientFor(cmd).AddProjectGroupMember(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added %s to group %s (members: %d)\n", args[1], g.Name, len(g.ProjectIDs))
			return nil
		},
	}
	return cmd
}

func newProjectGroupsMembersRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <group-id> <project-id>",
		Short: "Remove a project from a project group",
		Long:  "Incrementally remove a project from a project group.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			g, err := clientFor(cmd).RemoveProjectGroupMember(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s from group %s (members: %d)\n", args[1], g.Name, len(g.ProjectIDs))
			return nil
		},
	}
	return cmd
}
