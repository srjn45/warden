package metrics

import "testing"

func TestParseEtime(t *testing.T) {
	cases := map[string]int64{
		"05:09":      309,    // mm:ss
		"01:02:03":   3723,   // hh:mm:ss
		"2-03:04:05": 183845, // dd-hh:mm:ss
		"00:00":      0,
	}
	for in, want := range cases {
		if got := parseEtime(in); got != want {
			t.Fatalf("parseEtime(%q)=%d want %d", in, got, want)
		}
	}
}

func TestParsePSTable(t *testing.T) {
	// Columns: pid ppid rss(KiB) pcpu etime — output of
	// `ps -axo pid=,ppid=,rss=,pcpu=,etime=`.
	raw := "  100     1  20480   1.5    05:09\n" +
		"  200   100  51200  12.0 01:02:03\n" +
		"  300   200   1024   0.0    00:30\n"
	tbl := parsePSTable(raw)
	if len(tbl) != 3 {
		t.Fatalf("rows=%d want 3", len(tbl))
	}
	r := tbl[200]
	if r.PPID != 100 || r.RSSKiB != 51200 || r.CPU != 12.0 || r.EtimeSec != 3723 {
		t.Fatalf("row 200 = %+v", r)
	}
}

func TestAggregateTree(t *testing.T) {
	raw := "  100     1  20480   1.5    05:09\n" +
		"  200   100  51200  12.0    05:00\n" + // child of 100
		"  300   200   1024   0.5    04:00\n" + // grandchild
		"  900     1   8000   0.0    00:10\n" // unrelated
	tbl := parsePSTable(raw)
	rss, cpu, procs, uptime := aggregateTree(tbl, []int{100})
	if rss != (20480+51200+1024)*1024 {
		t.Fatalf("rss=%d", rss)
	}
	if cpu != 14.0 || procs != 3 || uptime != 309 {
		t.Fatalf("cpu=%v procs=%d uptime=%d", cpu, procs, uptime)
	}
}

func TestAggregateTreeMissingRoot(t *testing.T) {
	tbl := parsePSTable("  100   1  2048  0.0  00:05\n")
	rss, _, procs, _ := aggregateTree(tbl, []int{999})
	if rss != 0 || procs != 0 {
		t.Fatalf("missing root should aggregate to zero, got rss=%d procs=%d", rss, procs)
	}
}
