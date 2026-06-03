package tui

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChooseClassic(t *testing.T) {
	cases := []struct {
		name                                   string
		classicFlag, tmuxAvailable, insideTmux bool
		want                                   bool
	}{
		{"default composited", false, true, false, false},
		{"explicit --classic", true, true, false, true},
		{"no tmux falls back", false, false, false, true},
		{"inside tmux falls back", false, true, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, chooseClassic(c.classicFlag, c.tmuxAvailable, c.insideTmux))
		})
	}
}

func TestCockpitNames(t *testing.T) {
	require.Equal(t, "agentctl-tui-1234", cockpitSession(1234))
	require.Equal(t, filepath.Join("/run/agentctl", "tui-1234"), cockpitStateDir("/run/agentctl", 1234))
}
