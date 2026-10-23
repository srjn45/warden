package lifecycle

import (
	"context"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/tmuxproc"
)

type directStubHost struct {
	tmuxproc.Host
	plan        bool
	launched    bool
	argv        []string
	cwd         string
	env         []string
	newSessions int
	sent        []string
}

func (h *directStubHost) PlanAgentDirect(string) bool { return h.plan }
func (h *directStubHost) LaunchCommand(_ context.Context, _, cwd string, argv []string, env ...string) error {
	h.launched, h.cwd, h.argv, h.env = true, cwd, argv, env
	return nil
}
func (h *directStubHost) HostedDirectly(string) bool { return h.launched }
func (h *directStubHost) PinnedMode(string) string   { return "on" }
func (h *directStubHost) NewSession(context.Context, string, string, ...string) error {
	h.newSessions++
	return nil
}
func (h *directStubHost) CapturePane(context.Context, string) (string, error) {
	return "FAKE-CLI ready", nil
}
func (h *directStubHost) SendKeys(_ context.Context, _ string, keys ...string) error {
	h.sent = append(h.sent, strings.Join(keys, "|"))
	return nil
}

func TestDirectAgentLaunchIsHostAware(t *testing.T) {
	h := &directStubHost{plan: true}
	l := New(nil, nil)
	l.SetProcessHost(h)
	ctx := context.Background()
	if err := l.newAgentSession(ctx, "", "a1", "/work", "K=v"); err != nil {
		t.Fatal(err)
	}
	if h.newSessions != 0 || !l.agentDirect("a1") {
		t.Fatal("direct plan must defer the session instead of creating a tmux pane")
	}
	if l.agentExitSuffix("a1") != "" {
		t.Fatal("no shell exit-capture suffix for a directly hosted agent")
	}
	agent := &agentstore.Agent{ID: "a1"}
	if err := l.launchAgent(ctx, "", agent, "claude --model x"); err != nil {
		t.Fatal(err)
	}
	if h.cwd != "/work" || len(h.env) != 1 || h.env[0] != "K=v" || h.argv[len(h.argv)-1] != "claude --model x" {
		t.Fatalf("workdir/env/args not preserved: %+v", h)
	}
	if agent.Runtime == nil || agent.Runtime.Kind != "direct" || agent.Runtime.PinnedMode != "on" {
		t.Fatalf("runtime identity not stamped direct: %+v", agent.Runtime)
	}
	if out, err := l.Output(ctx, "a1", 5); err != nil || !strings.Contains(out, "FAKE-CLI") {
		t.Fatalf("output via host: %q %v", out, err)
	}
	if err := l.Input(ctx, "a1", "do it"); err != nil || len(h.sent) != 2 || !strings.Contains(h.sent[0], "do it") || h.sent[1] != "Enter" {
		t.Fatalf("seed input via host: %v %v", h.sent, err)
	}
}

func TestAgentLaunchStaysTmuxWhenNotPlanned(t *testing.T) {
	h := &directStubHost{plan: false}
	l := New(nil, nil)
	l.SetProcessHost(h)
	if err := l.newAgentSession(context.Background(), "", "a1", "/work"); err != nil {
		t.Fatal(err)
	}
	if h.newSessions != 1 || l.agentDirect("a1") {
		t.Fatal("unplanned agent must use the unchanged tmux path")
	}
}
