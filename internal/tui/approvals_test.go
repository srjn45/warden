package tui

import (
	"strings"
	"testing"

	"github.com/srajanpathak/agentctl/internal/approval"
	"github.com/stretchr/testify/require"
)

func TestRenderApprovalsQueueRecognized(t *testing.T) {
	views := []approval.View{
		{ID: "a1", Action: "Bash(ls)", Question: "Do you want to proceed?", Options: []string{"Yes", "No"}, Recognized: true},
	}
	out := renderApprovalsQueue(views, 0, true, 60, 20)
	require.Contains(t, out, "a1")
	require.Contains(t, out, "Bash(ls)")
	require.Contains(t, out, "[1] Yes")
	require.Contains(t, out, "[2] No")
}

func TestRenderApprovalsQueueUnrecognized(t *testing.T) {
	views := []approval.View{{ID: "a2", Recognized: false}}
	out := renderApprovalsQueue(views, 0, true, 60, 20)
	require.Contains(t, strings.ToLower(out), "attach")
}

func TestRenderApprovalsQueueEmpty(t *testing.T) {
	out := renderApprovalsQueue(nil, 0, false, 60, 20)
	require.Contains(t, strings.ToLower(out), "nothing")
}
