package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/projectstore"
)

// newProjectsCmd builds the canonical projects namespace for first-class daemon
// projects (docs/specs/2026-09-05-project-groups-membership-and-cli.md).
// It is distinct from `warden project` (singular), which manages repo-local config.
func newProjectsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "projects",
		Short: "Inspect and manage daemon-registered projects",
		Long: "Inspect and manage first-class daemon projects.\n\n" +
			"Projects represent checkout roots or remote repositories tracked by the\n" +
			"warden daemon. Agents and pipelines associate with these projects.\n\n" +
			"Subcommands allow listing, opening (local or remote), creating, and closing projects.\n" +
			"(For repo-local configuration like memory or presets, use `warden project`.)",
	}
	SetCommandHelpMetadata(cmd, "project", 6, "warden projects", "", NodeNamespace)

	children := []*cobra.Command{
		newProjectsListCmd(),
		newProjectsOpenCmd(),
		newProjectsOpenLocalCmd(),
		newProjectsOpenRemoteCmd(),
		newProjectsNewCmd(),
		newProjectsCloseCmd(),
	}
	for i, child := range children {
		SetCommandHelpMetadata(child, "project", (i+1)*10, "warden projects "+child.Name(), "", nodeKind(child))
		cmd.AddCommand(child)
	}
	return cmd
}

func newProjectsListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List registered projects",
		Long:    "List every project registered with the daemon, including open and closed (hibernated) projects.",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projects, err := clientFor(cmd).ListProjects(cmd.Context())
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				if projects == nil {
					projects = []projectstore.Project{}
				}
				return printJSON(cmd.OutOrStdout(), projects)
			}
			if len(projects) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no projects found")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tPATH")
			for _, p := range projects {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.ID, p.Name, p.Status, p.Path)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func newProjectsOpenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "open <id>",
		Short: "Register or reopen a project by its canonical ID",
		Long:  "Register or reopen a project by its canonical ID (checkout path or remote URL).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _ := cmd.Flags().GetString("name")
			p, err := clientFor(cmd).OpenProject(cmd.Context(), args[0], name)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "opened project %s (%s)\n", p.ID, p.Status)
			return nil
		},
	}
	cmd.Flags().String("name", "", "optional display name")
	return cmd
}

func newProjectsOpenLocalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "open-local <path>",
		Short: "Register an existing local directory as a project",
		Long:  "Register an existing local directory as a project. The daemon normalizes the path and restores any hibernated agents.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _ := cmd.Flags().GetString("name")
			p, err := clientFor(cmd).OpenLocalProject(cmd.Context(), args[0], name)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "opened local project %s (%s)\n", p.ID, p.Status)
			return nil
		},
	}
	cmd.Flags().String("name", "", "optional display name")
	return cmd
}

func newProjectsOpenRemoteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "open-remote <url>",
		Short: "Clone a remote Git repository and register it as a project",
		Long:  "Clone a remote Git repository into the daemon workspace and register it as a project.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _ := cmd.Flags().GetString("name")
			p, err := clientFor(cmd).OpenRemoteProject(cmd.Context(), args[0], name)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "opened remote project %s (%s)\n", p.ID, p.Status)
			return nil
		},
	}
	cmd.Flags().String("name", "", "optional display name")
	return cmd
}

func newProjectsNewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Scaffold a brand-new project and register it",
		Long:  "Scaffold a brand-new project with git init and initial commit in the daemon workspace, and register it.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := clientFor(cmd).CreateProject(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created project %s (%s)\n", p.ID, p.Status)
			return nil
		},
	}
	return cmd
}

func newProjectsCloseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "close <id>",
		Short: "Hibernate a project and gracefully stop its active agents",
		Long:  "Hibernate a project: keep the record in the store, set its status to closed, and gracefully stop its live agents.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := clientFor(cmd).CloseProject(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "closed project %s (%s)\n", p.ID, p.Status)
			return nil
		},
	}
	return cmd
}
