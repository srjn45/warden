package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestCompletionCommand(t *testing.T) {
	tests := []struct {
		shell      string
		wantInHelp string
	}{
		{"bash", "/etc/bash_completion.d/warden"},
		{"zsh", "/usr/local/share/zsh/site-functions/_warden"},
		{"fish", "~/.config/fish/completions/warden.fish"},
		{"powershell", "warden.ps1"},
	}

	for _, tt := range tests {
		t.Run(tt.shell, func(t *testing.T) {
			root := newRootCmd()
			cmd := root.Commands()
			var found *cobra.Command
			for _, c := range cmd {
				if c.Use == "completion" {
					found = c
					break
				}
			}
			if found == nil {
				t.Fatal("completion command not registered")
			}

			// Check that shell-specific subcommand exists
			var shellCmd *cobra.Command
			for _, c := range found.Commands() {
				if c.Use == tt.shell {
					shellCmd = c
					break
				}
			}
			if shellCmd == nil {
				t.Fatalf("completion %s subcommand not found", tt.shell)
			}

			// Check help text contains install path
			if !strings.Contains(found.Long, tt.wantInHelp) {
				t.Errorf("completion help missing %q for %s", tt.wantInHelp, tt.shell)
			}
		})
	}
}

func TestBashCompletionGenerates(t *testing.T) {
	root := newRootCmd()
	buf := new(bytes.Buffer)

	// Call GenBashCompletion directly to generate bash completion script
	if err := root.GenBashCompletion(buf); err != nil {
		t.Fatalf("GenBashCompletion failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "# bash completion") {
		t.Error("bash completion output missing expected header")
	}
	if !strings.Contains(output, "warden") {
		t.Error("bash completion output missing warden command")
	}
}
