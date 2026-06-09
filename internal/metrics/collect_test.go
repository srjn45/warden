package metrics

import (
	"context"
	"testing"
)

// fakeRunner returns canned output per "name arg0 arg1 ..." key, like
// lifecycle.FakeRunner but local so this package needn't import lifecycle.
type fakeRunner struct{ resp map[string]string }

func (f fakeRunner) Run(_ context.Context, _ string, name string, args ...string) (string, error) {
	key := name
	for _, a := range args {
		key += " " + a
	}
	return f.resp[key], nil
}

type fakeLister struct{ agents []Agent }

func (f fakeLister) LiveAgents(_ context.Context) ([]Agent, error) { return f.agents, nil }

func TestCollectorSample(t *testing.T) {
	ps := "  100     1  20480   1.0    10:00\n" + // agent A pane (pid 100)
		"  101   100  51200   5.0    09:00\n" + // claude child of A
		"  200     1  10240   0.5    05:00\n" + // agent B pane (pid 200)
		"  999     1  30000   0.0    01:00\n" // the daemon itself (self pid)
	c := &Collector{
		Run: fakeRunner{resp: map[string]string{
			"ps -axo pid=,ppid=,rss=,pcpu=,etime=":      ps,
			"tmux list-panes -F #{pane_pid} -t agent-a": "100\n",
			"tmux list-panes -F #{pane_pid} -t agent-b": "200\n",
			"vm_stat":                "Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free: 100.\n",
			"sysctl -n hw.memsize":   "17179869184\n",
			"sysctl -n vm.swapusage": "vm.swapusage: total = 2048.00M used = 256.00M free = 1792.00M",
		}}.Run,
		Lister:   fakeLister{agents: []Agent{{ID: "agent-a", TmuxSession: "agent-a", Status: "working"}, {ID: "agent-b", TmuxSession: "agent-b", Status: "idle"}}},
		SelfPID:  999,
		Pressure: func() string { return "warn" },
	}
	s, err := c.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Agents) != 2 {
		t.Fatalf("agents=%d want 2", len(s.Agents))
	}
	a := s.Agents[0] // agent-a: pane 100 + child 101
	if !a.Paneable || a.RSSBytes != (20480+51200)*1024 || a.ProcCount != 2 || a.CPUPercent != 6.0 {
		t.Fatalf("agent-a = %+v", a)
	}
	if s.System.AgentCount != 2 || s.System.AttributedRSSBytes != (20480+51200+10240)*1024 {
		t.Fatalf("system totals wrong: %+v", s.System)
	}
	if s.System.PressureLevel != "warn" || s.System.TotalBytes != 17179869184 {
		t.Fatalf("system mem wrong: %+v", s.System)
	}
	if s.Daemon.RSSBytes != 30000*1024 || s.Daemon.Goroutines < 1 {
		t.Fatalf("daemon stats wrong: %+v", s.Daemon)
	}
}

func TestCollectorAgentDegradesWhenPaneMissing(t *testing.T) {
	c := &Collector{
		Run: fakeRunner{resp: map[string]string{
			"ps -axo pid=,ppid=,rss=,pcpu=,etime=": "  1  0  100  0.0  00:01\n",
			// no tmux list-panes entry → empty pane list → non-paneable
		}}.Run,
		Lister:   fakeLister{agents: []Agent{{ID: "ghost", TmuxSession: "ghost", Status: "orphaned"}}},
		SelfPID:  1,
		Pressure: func() string { return "normal" },
	}
	s, err := c.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Agents) != 1 || s.Agents[0].Paneable || s.Agents[0].RSSBytes != 0 {
		t.Fatalf("ghost agent should be non-paneable zero: %+v", s.Agents)
	}
}

func TestCollectorMultiPaneAgent(t *testing.T) {
	// An agent with two panes (two root pids) aggregates both subtrees.
	ps := "  10   1  1000  0.0  01:00\n" +
		"  20   1  2000  0.0  01:00\n"
	c := &Collector{
		Run: fakeRunner{resp: map[string]string{
			"ps -axo pid=,ppid=,rss=,pcpu=,etime=":    ps,
			"tmux list-panes -F #{pane_pid} -t multi": "10\n20\n",
		}}.Run,
		Lister:   fakeLister{agents: []Agent{{ID: "multi", TmuxSession: "multi", Status: "working"}}},
		SelfPID:  1,
		Pressure: func() string { return "normal" },
	}
	s, err := c.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Agents) != 1 || !s.Agents[0].Paneable || s.Agents[0].ProcCount != 2 || s.Agents[0].RSSBytes != (1000+2000)*1024 {
		t.Fatalf("multi-pane agent = %+v", s.Agents)
	}
}

func TestOpenFDCountViaLsof(t *testing.T) {
	// pid 999999999 has no /proc/<pid>/fd on any OS, so this exercises the lsof
	// fallback deterministically (macOS + Linux CI alike).
	c := &Collector{Run: fakeRunner{resp: map[string]string{
		"lsof -wnP -p 999999999 -F f": "p1\nfcwd\nftxt\nf0\nf1\nf2\nf7\n",
	}}.Run}
	if got := c.openFDCount(context.Background(), 999999999); got != 4 {
		t.Fatalf("openFDCount = %d, want 4 (f0,f1,f2,f7)", got)
	}
}
