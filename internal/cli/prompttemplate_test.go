package cli

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/prompttemplate"
	"github.com/stretchr/testify/require"
)

// runPromptTemplate executes a `prompt-template` subcommand with --config
// pointed at a config path (prompt-templates.yaml is derived from its
// directory), returning combined output.
func runPromptTemplate(t *testing.T, configPath string, args ...string) string {
	t.Helper()
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"prompt-template"}, append(args, "--config", configPath)...))
	require.NoError(t, root.Execute())
	return buf.String()
}

func TestParseSetVars(t *testing.T) {
	got, err := parseSetVars([]string{"FILE=foo.go", "X=y"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"FILE": "foo.go", "X": "y"}, got)

	// A value may itself contain '='; only the first separator splits.
	got, err = parseSetVars([]string{"EXPR=a=b"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"EXPR": "a=b"}, got)

	_, err = parseSetVars([]string{"noseparator"})
	require.Error(t, err)
	_, err = parseSetVars([]string{"=novar"})
	require.Error(t, err)
}

// TestPromptTemplateSaveAndList persists a template (vars auto-derived from the
// body) and shows it in `prompt-template list`.
func TestPromptTemplateSaveAndList(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")

	out := runPromptTemplate(t, cfg, "save", "bugfix", "--prompt", "Fix {{FILE}} for {{TICKET}}")
	require.Contains(t, out, "saved prompt template")
	require.Contains(t, out, "bugfix")

	store, err := prompttemplate.Load(filepath.Join(filepath.Dir(cfg), "prompt-templates.yaml"))
	require.NoError(t, err)
	tpl, ok := store.Get("bugfix")
	require.True(t, ok)
	require.Equal(t, "Fix {{FILE}} for {{TICKET}}", tpl.Prompt)
	require.Equal(t, []string{"FILE", "TICKET"}, tpl.Vars)

	list := runPromptTemplate(t, cfg, "list")
	require.Contains(t, list, "bugfix")
	require.Contains(t, list, "FILE, TICKET")
}

func TestPromptTemplateListEmpty(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	out := runPromptTemplate(t, cfg, "list")
	require.Contains(t, out, "no prompt templates saved")
}

func TestPromptTemplateFlagsRegistered(t *testing.T) {
	cmd := newStartCmd()
	require.NotNil(t, cmd.Flags().Lookup("prompt-template"), "--prompt-template must be registered on start")
	require.NotNil(t, cmd.Flags().Lookup("set"), "--set must be registered on start")
}

// startCmdWithConfig builds a start command with a --config flag so the
// prompt-template store path resolves beside the given config path (mirrors how
// the persistent root flag propagates at runtime).
func startCmdWithConfig(configPath string) *cobra.Command {
	cmd := newStartCmd()
	cmd.Flags().String("config", configPath, "")
	return cmd
}

func TestResolveStartPromptPositionalWins(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	store := &prompttemplate.Store{}
	store.Set("t", prompttemplate.Template{Prompt: "from template", Vars: nil})
	require.NoError(t, store.Save(filepath.Join(filepath.Dir(cfg), "prompt-templates.yaml")))

	cmd := startCmdWithConfig(cfg)
	require.NoError(t, cmd.Flags().Set("prompt-template", "t"))
	got, err := resolveStartPrompt(cmd, []string{"explicit prompt"})
	require.NoError(t, err)
	require.Equal(t, "explicit prompt", got, "an explicit positional prompt overrides the template")
}

func TestResolveStartPromptFillsTemplate(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	store := &prompttemplate.Store{}
	store.Set("refactor", prompttemplate.Template{Prompt: "Refactor {{FILE}} to use {{LIB}}", Vars: []string{"FILE", "LIB"}})
	require.NoError(t, store.Save(filepath.Join(filepath.Dir(cfg), "prompt-templates.yaml")))

	cmd := startCmdWithConfig(cfg)
	require.NoError(t, cmd.Flags().Set("prompt-template", "refactor"))
	require.NoError(t, cmd.Flags().Set("set", "FILE=foo.go"))
	require.NoError(t, cmd.Flags().Set("set", "LIB=errgroup"))
	got, err := resolveStartPrompt(cmd, nil)
	require.NoError(t, err)
	require.Equal(t, "Refactor foo.go to use errgroup", got)
}

func TestResolveStartPromptMissingTemplateErrors(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	cmd := startCmdWithConfig(cfg)
	require.NoError(t, cmd.Flags().Set("prompt-template", "ghost"))
	_, err := resolveStartPrompt(cmd, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestResolveStartPromptNoTemplateNoPrompt(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	cmd := startCmdWithConfig(cfg)
	got, err := resolveStartPrompt(cmd, nil)
	require.NoError(t, err)
	require.Equal(t, "", got, "no positional prompt and no template means an interactive spawn")
}
