package cli

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestIsCommandNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "exec.ErrNotFound",
			err:  exec.ErrNotFound,
			want: true,
		},
		{
			name: "wrapped exec.ErrNotFound",
			err:  errors.New("some context: executable file not found in $PATH"),
			want: true,
		},
		{
			name: "other error",
			err:  errors.New("permission denied"),
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCommandNotFound(tt.err); got != tt.want {
				t.Errorf("isCommandNotFound() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInstallHint(t *testing.T) {
	tests := []struct {
		name    string
		cmd     string
		wantMac string
		wantLin string
	}{
		{
			name:    "tmux",
			cmd:     "tmux",
			wantMac: "brew install tmux",
			wantLin: "apt install tmux",
		},
		{
			name:    "gh",
			cmd:     "gh",
			wantMac: "brew install gh",
			wantLin: "apt install gh",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hint := installHint(tt.cmd)
			if !strings.Contains(hint, tt.wantMac) {
				t.Errorf("installHint(%q) missing macOS hint %q", tt.cmd, tt.wantMac)
			}
			if !strings.Contains(hint, tt.wantLin) {
				t.Errorf("installHint(%q) missing Linux hint %q", tt.cmd, tt.wantLin)
			}
		})
	}
}
