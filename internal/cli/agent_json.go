package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/store"
)

// addJSONFlag registers the shared --json flag on a scriptable agent command.
func addJSONFlag(cmd *cobra.Command, usage string) {
	cmd.Flags().Bool("json", false, usage)
}

// jsonRequested reports whether --json was set on cmd.
func jsonRequested(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool("json")
	return v
}

// progressOut is where human-readable progress goes: stderr under --json (stdout
// carries only the JSON document), stdout otherwise.
func progressOut(cmd *cobra.Command) io.Writer {
	if jsonRequested(cmd) {
		return cmd.ErrOrStderr()
	}
	return cmd.OutOrStdout()
}

// spawnedAgentJSON is the machine-readable shape of a newly spawned agent
// (start / fork / handoff), taken from the daemon's returned session.
type spawnedAgentJSON struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Role    string `json:"role"`
	AiCli   string `json:"ai_cli"`
	Model   string `json:"model"`
	Workdir string `json:"workdir"`
	Status  string `json:"status,omitempty"`
}

func newSpawnedAgentJSON(s *store.Session) spawnedAgentJSON {
	aiCli := s.AiCli
	if aiCli == "" {
		aiCli = s.Backend
	}
	return spawnedAgentJSON{
		ID: s.ID, Name: s.Name, Role: store.DisplayRole(s.Role, s.Type),
		AiCli: aiCli, Model: s.Model, Workdir: s.Workdir, Status: string(s.Status),
	}
}

// printSpawnedJSON writes the spawned-agent document to stdout.
func printSpawnedJSON(cmd *cobra.Command, s *store.Session) error {
	return printJSON(cmd.OutOrStdout(), newSpawnedAgentJSON(s))
}

// teardownResult records which teardown steps ran, for `stop --json`.
type teardownResult struct {
	ID    string   `json:"id"`
	Steps []string `json:"steps"`
	PRURL string   `json:"pr_url,omitempty"`
}

// requireYesForJSON fails when --json would need to prompt: stdout must stay a
// single JSON document, so an unconfirmed destructive step is refused instead.
func requireYesForJSON(_ *cobra.Command, what string) error {
	return fmt.Errorf("--json cannot prompt for confirmation to %s; re-run with --yes", what)
}
