package autopilot

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// InitConfig parameterizes the one-command adoption scaffolder (autopilot.md §5.1).
type InitConfig struct {
	// ConfigPath is the warden config file to update with the autopilot block.
	// If empty the config update step is skipped.
	ConfigPath string
	// PlanFile is the plan file path relative to repo (default "autopilot.plan.yaml").
	PlanFile string
	// Name is the registered plan name (default "default").
	Name string
	// Register persists the newly scaffolded plan in the daemon run store.
	// It is optional for embedders and tests that only need filesystem setup.
	Register func(context.Context, RegisterRequest) error
	// IntegrationBranch is the merge-target template (config
	// autopilot.merge.target_branch, default "autopilot/integration"). Empty and
	// the legacy default derive autopilot/<plan-name>; {{plan}} is expanded.
	IntegrationBranch string
	// Backends is retained for source compatibility. Backend tiers now live in
	// the backend registry and are not written to config.
	Backends []string
}

// planTemplate is the starting point the owner edits before enabling autopilot.
const planTemplate = `version: 1
goal: "TODO: describe the goal autopilot should work toward"
constraints: []
# tasks:       # optional; the brain authors and refines them when absent
#   - id: feature
#     prompt: "Implement the feature per docs/specs/feature.md"
# done_when:   # optional acceptance criteria the brain verifies before declaring complete
#   - "wd check passes on autopilot/integration"
`

// Init scaffolds autopilot adoption in one command (autopilot.md §5.1):
//   - writes a template plan file in repo if absent
//   - updates the autopilot block in the warden config (plans + detected backends)
//   - creates the integration branch off the default branch if absent
//   - prints a CI-coverage hint when workflows do not cover integration PRs
func Init(ctx context.Context, env Env, repo string, cfg InitConfig, out io.Writer) error {
	if cfg.PlanFile == "" {
		if cfg.Name == "" {
			cfg.Name = "default"
		}
		if !validPlanName(cfg.Name) {
			return fmt.Errorf("invalid plan name %q (use letters, numbers, '.', '_' or '-')", cfg.Name)
		}
		cfg.PlanFile = filepath.Join("plans", cfg.Name+".yaml")
	}
	if cfg.Name == "" {
		cfg.Name = defaultRunName(cfg.PlanFile)
	}
	branch, err := ResolveInitIntegrationBranch(cfg.Name, cfg.IntegrationBranch)
	if err != nil {
		return err
	}
	cfg.IntegrationBranch = branch

	if err := writePlanIfAbsent(filepath.Join(repo, cfg.PlanFile), out); err != nil {
		return err
	}

	if cfg.Register != nil {
		if err := cfg.Register(ctx, RegisterRequest{Name: cfg.Name, Repo: repo, PlanFile: filepath.Join(repo, cfg.PlanFile)}); err != nil {
			return fmt.Errorf("register autopilot plan: %w", err)
		}
		fmt.Fprintf(out, "✓ registered autopilot plan %s\n", cfg.Name)
	}

	if err := ensureIntegrationBranch(ctx, env, repo, cfg.IntegrationBranch, out); err != nil {
		fmt.Fprintf(out, "  warning: integration branch: %v\n", err)
	}

	printCIHint(repo, cfg.IntegrationBranch, out)

	fmt.Fprintf(out, "\nnext: edit %s, then run `warden autopilot start %s`\n", cfg.PlanFile, cfg.Name)
	return nil
}

func validPlanName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// writePlanIfAbsent writes the template plan file when it does not already exist.
func writePlanIfAbsent(path string, out io.Writer) error {
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(out, "  %s already exists — skipped\n", filepath.Base(path))
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create plan directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(planTemplate), 0o644); err != nil {
		return fmt.Errorf("write plan template %s: %w", path, err)
	}
	fmt.Fprintf(out, "✓ created %s — edit the goal before enabling\n", filepath.Base(path))
	return nil
}

// updateAutopilotConfig updates the autopilot.plans and brain.backends sub-keys
// in the warden config file at path. It is idempotent: if plans is already
// non-empty the file is left unchanged.
func updateAutopilotConfig(path, planFile string, backends []string, out io.Writer) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(out, "  config %s not found — start the daemon once to initialise it\n", path)
			return nil
		}
		return err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		fmt.Fprintf(out, "  unexpected config shape in %s — skipped\n", path)
		return nil
	}
	root := doc.Content[0]

	apNode := yamlValueOf(root, "autopilot")
	if apNode == nil || apNode.Kind != yaml.MappingNode {
		fmt.Fprintf(out, "  no autopilot block in %s — start the daemon once to initialise it\n", path)
		return nil
	}

	plansNode := yamlValueOf(apNode, "plans")
	if plansNode != nil && plansNode.Kind == yaml.SequenceNode && len(plansNode.Content) > 0 {
		fmt.Fprintf(out, "  autopilot.plans already set in %s — skipped\n", path)
		return nil
	}

	// Update plans list.
	planEntry := buildPlanEntry(planFile)
	if plansNode != nil {
		plansNode.Kind = yaml.SequenceNode
		plansNode.Tag = "!!seq"
		plansNode.Style = 0
		plansNode.Value = ""
		plansNode.Content = []*yaml.Node{planEntry}
	}

	// Update brain.backends from detected installations.
	if len(backends) > 0 {
		brainNode := yamlValueOf(apNode, "brain")
		if brainNode != nil && brainNode.Kind == yaml.MappingNode {
			backendsNode := yamlValueOf(brainNode, "backends")
			if backendsNode != nil && backendsNode.Kind == yaml.MappingNode {
				setDetectedBackends(backendsNode, backends)
			}
		}
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		_ = enc.Close()
		return fmt.Errorf("encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("encode close: %w", err)
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	fmt.Fprintf(out, "✓ updated autopilot block in %s (assign backends to cost tiers)\n", path)
	return nil
}

// yamlValueOf finds the value node for key in a YAML mapping node, or nil.
func yamlValueOf(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func buildPlanEntry(file string) *yaml.Node {
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = []*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "file"},
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: file},
	}
	return m
}

// setDetectedBackends distributes detected backend ids across the cost-tier
// sub-nodes of a brain.backends mapping. The heuristic: claude → subscription;
// everything else → free. The owner is expected to review and reassign.
func setDetectedBackends(backendsNode *yaml.Node, detected []string) {
	var sub, free []string
	for _, id := range detected {
		if id == "claude" {
			sub = append(sub, id)
		} else {
			free = append(free, id)
		}
	}
	if v := yamlValueOf(backendsNode, "free"); v != nil && v.Kind == yaml.SequenceNode {
		v.Content = strSliceToNodes(free)
	}
	if v := yamlValueOf(backendsNode, "subscription"); v != nil && v.Kind == yaml.SequenceNode {
		v.Content = strSliceToNodes(sub)
	}
}

func strSliceToNodes(ss []string) []*yaml.Node {
	nodes := make([]*yaml.Node, len(ss))
	for i, s := range ss {
		nodes[i] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
	}
	return nodes
}

// ensureIntegrationBranch creates the integration branch off the repo's default
// branch if it does not already exist.
func ensureIntegrationBranch(ctx context.Context, env Env, repo, branch string, out io.Writer) error {
	def, err := env.DefaultBranch(ctx, repo)
	if err != nil {
		return fmt.Errorf("resolve default branch: %w", err)
	}
	if isProtectedBranch(branch, def) {
		return fmt.Errorf("integration branch %q is a protected name — choose a dedicated branch (e.g. autopilot/<plan-name>)", branch)
	}
	exists, err := env.BranchExists(ctx, repo, branch)
	if err != nil {
		return fmt.Errorf("check branch: %w", err)
	}
	if exists {
		fmt.Fprintf(out, "  branch %s already exists — skipped\n", branch)
		return nil
	}
	if err := env.CreateBranch(ctx, repo, branch, def); err != nil {
		return fmt.Errorf("create branch %s off %s: %w", branch, def, err)
	}
	fmt.Fprintf(out, "✓ created integration branch %s off %s\n", branch, def)
	return nil
}

// ciCoversIntegration reports whether any GitHub Actions workflow in repo has a
// pull_request trigger that would fire for branch. Returns false when the
// .github/workflows directory is absent or no matching workflow is found.
func ciCoversIntegration(repo, branch string) bool {
	dir := filepath.Join(repo, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(n, ".yml") && !strings.HasSuffix(n, ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		if workflowCoversBranch(data, branch) {
			return true
		}
	}
	return false
}

// workflowCoversBranch checks whether a single workflow YAML covers pull
// requests against branch.
func workflowCoversBranch(data []byte, branch string) bool {
	var wf struct {
		On yaml.Node `yaml:"on"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil || wf.On.Kind == 0 {
		return false
	}
	return onNodeCoversBranch(&wf.On, branch)
}

// onNodeCoversBranch interprets the "on:" node of a GitHub Actions workflow.
// Supported forms: scalar "pull_request", sequence containing "pull_request",
// and mapping {pull_request: {branches: [...]}} with optional branch filters.
func onNodeCoversBranch(on *yaml.Node, branch string) bool {
	switch on.Kind {
	case yaml.ScalarNode:
		return on.Value == "pull_request"
	case yaml.SequenceNode:
		for _, c := range on.Content {
			if c.Kind == yaml.ScalarNode && c.Value == "pull_request" {
				return true
			}
		}
	case yaml.MappingNode:
		pr := yamlValueOf(on, "pull_request")
		if pr == nil {
			return false
		}
		// pull_request with no value (null / empty mapping) = all branches
		if pr.Kind != yaml.MappingNode || len(pr.Content) == 0 {
			return true
		}
		branchesNode := yamlValueOf(pr, "branches")
		if branchesNode == nil {
			return true // pull_request with no branches filter covers all
		}
		if branchesNode.Kind != yaml.SequenceNode {
			return false
		}
		for _, b := range branchesNode.Content {
			if branchGlobMatches(b.Value, branch) {
				return true
			}
		}
	}
	return false
}

// branchGlobMatches returns true when pattern (GitHub Actions branch glob) matches
// branch. Supports exact match, "**" wildcard, and trailing "/**" prefix match.
func branchGlobMatches(pattern, branch string) bool {
	if pattern == "**" || pattern == "*" || pattern == branch {
		return true
	}
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return strings.HasPrefix(branch, prefix+"/")
	}
	if strings.HasSuffix(pattern, "/*") {
		prefix := strings.TrimSuffix(pattern, "/*")
		rest := strings.TrimPrefix(branch, prefix+"/")
		return rest != branch && !strings.Contains(rest, "/")
	}
	return false
}

func printCIHint(repo, branch string, out io.Writer) {
	if !ciCoversIntegration(repo, branch) {
		fmt.Fprintf(out, "\nhint: no CI workflow found that covers %s pull requests\n", branch)
		fmt.Fprintf(out, "      autopilot will use gate: local (project checks instead of CI)\n")
		fmt.Fprintf(out, "      to enable CI gating, add %q to on.pull_request.branches in\n", "autopilot/**")
		fmt.Fprintf(out, "      one of your .github/workflows/*.yml files (covers every per-plan branch)\n")
	}
}
