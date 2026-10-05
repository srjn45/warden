package cli

import (
	"bytes"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/client"
	"gopkg.in/yaml.v3"
)

func newPlanEditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "edit <plan-id>",
		Short: "Edit a pending plan definition in $EDITOR",
		Long: "Fetch a pending plan's definition, open it as YAML in $EDITOR (or $VISUAL,\n" +
			"falling back to vi), then apply the saved document as the new definition.\n\n" +
			"Only name, goal, constraints, done_when, and tasks are written/applied.\n" +
			"Lifecycle and execution fields are ignored. If the editor exits non-zero or\n" +
			"the file is unchanged, no API call is made. Optimistic concurrency uses the\n" +
			"revision observed at fetch time.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlanEdit(cmd, args[0])
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func runPlanEdit(cmd *cobra.Command, planID string) error {
	c := clientFor(cmd)
	cur, err := c.PlansGet(cmd.Context(), planID)
	if err != nil {
		return err
	}

	raw, err := marshalPlanEditYAML(cur)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp("", "warden-plan-edit-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	before, err := os.ReadFile(tmpPath)
	if err != nil {
		return fmt.Errorf("read temp file: %w", err)
	}
	if err := openEditor(cmd, tmpPath); err != nil {
		return fmt.Errorf("editor: %w", err)
	}
	after, err := os.ReadFile(tmpPath)
	if err != nil {
		return fmt.Errorf("read edited file: %w", err)
	}
	if bytes.Equal(before, after) {
		fmt.Fprintln(cmd.OutOrStdout(), "no changes — aborted")
		return nil
	}

	req, err := client.ParsePlanYAML(after)
	if err != nil {
		return err
	}
	req.ExpectedRevision = cur.Revision

	p, err := c.PlansUpdate(cmd.Context(), planID, req)
	if err != nil {
		return err
	}
	if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
		return printJSON(cmd.OutOrStdout(), p)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "updated plan %s (%s) rev=%d hash=%s\n", p.ID, p.Name, p.Revision, p.ContentHash)
	return nil
}

// planEditDoc is the YAML shape written for `wd plan edit`.
type planEditDoc struct {
	Name        string            `yaml:"name"`
	Goal        string            `yaml:"goal"`
	Constraints []string          `yaml:"constraints"`
	DoneWhen    []string          `yaml:"done_when"`
	Tasks       []planEditTaskDoc `yaml:"tasks"`
}

type planEditTaskDoc struct {
	ID     string   `yaml:"id"`
	Prompt string   `yaml:"prompt"`
	After  []string `yaml:"after,omitempty"`
}

func marshalPlanEditYAML(p *client.PlanView) ([]byte, error) {
	doc := planEditDoc{
		Name:        p.Name,
		Goal:        p.Goal,
		Constraints: append([]string(nil), p.Constraints...),
		DoneWhen:    append([]string(nil), p.DoneWhen...),
	}
	if doc.Constraints == nil {
		doc.Constraints = []string{}
	}
	if doc.DoneWhen == nil {
		doc.DoneWhen = []string{}
	}
	for _, t := range p.Tasks {
		doc.Tasks = append(doc.Tasks, planEditTaskDoc{
			ID:     t.ID,
			Prompt: t.Prompt,
			After:  append([]string(nil), t.After...),
		})
	}
	if doc.Tasks == nil {
		doc.Tasks = []planEditTaskDoc{}
	}

	var buf bytes.Buffer
	buf.WriteString("# Edit the pending plan definition below, then save and quit.\n")
	buf.WriteString("# Applied fields: name, goal, constraints, done_when, tasks (id/prompt/after).\n")
	buf.WriteString("# Lifecycle/execution fields are ignored. Quit without saving to abort.\n\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode plan yaml: %w", err)
	}
	_ = enc.Close()
	return buf.Bytes(), nil
}
