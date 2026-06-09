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

func TestParseVMStat(t *testing.T) {
	raw := "Mach Virtual Memory Statistics: (page size of 16384 bytes)\n" +
		"Pages free:                          100.\n" +
		"Pages active:                        200.\n" +
		"Pages inactive:                       50.\n" +
		"Pages wired down:                     30.\n" +
		"Pages occupied by compressor:         40.\n"
	pageSize, counts := parseVMStat(raw)
	if pageSize != 16384 {
		t.Fatalf("pageSize=%d", pageSize)
	}
	if counts["Pages free"] != 100 || counts["Pages occupied by compressor"] != 40 {
		t.Fatalf("counts=%+v", counts)
	}
}

func TestParseSwapUsed(t *testing.T) {
	raw := "vm.swapusage: total = 2048.00M  used = 512.50M  free = 1535.50M  (encrypted)"
	got := parseSwapUsed(raw)
	want := uint64(512.5 * 1024 * 1024)
	if got != want {
		t.Fatalf("swap used=%d want %d", got, want)
	}
}

func TestParseMemSize(t *testing.T) {
	if got := parseMemSize("17179869184\n"); got != 17179869184 {
		t.Fatalf("memsize=%d", got)
	}
	if got := parseMemSize("hw.memsize: 17179869184"); got != 17179869184 {
		t.Fatalf("memsize with prefix=%d", got)
	}
}

func TestBuildSystemStats(t *testing.T) {
	counts := map[string]int64{
		"Pages free":                   100,
		"Pages wired down":             30,
		"Pages occupied by compressor": 40,
	}
	ss := buildSystemStats(16384, counts, 1<<24 /*16MiB total*/, 1024*1024, "warn")
	if ss.FreeBytes != 100*16384 || ss.WiredBytes != 30*16384 || ss.CompressedBytes != 40*16384 {
		t.Fatalf("ss=%+v", ss)
	}
	if ss.UsedBytes != ss.TotalBytes-ss.FreeBytes || ss.TotalBytes != 1<<24 {
		t.Fatalf("used/total wrong: %+v", ss)
	}
	if ss.SwapUsedBytes != 1024*1024 || ss.PressureLevel != "warn" {
		t.Fatalf("swap/pressure wrong: %+v", ss)
	}
}

func TestParseVMStatDegradesOnGarbage(t *testing.T) {
	// No header, no parseable lines → default page size, empty counts, no panic.
	pageSize, counts := parseVMStat("garbage\n\n")
	if pageSize != 4096 || len(counts) != 0 {
		t.Fatalf("pageSize=%d counts=%+v", pageSize, counts)
	}
}

func TestParseLsofFDCount(t *testing.T) {
	// `lsof -F f` output: p<pid> header, pseudo-fds (cwd/txt/mem/rtd), then
	// numbered fds. Only the numbered ones count.
	out := "p123\nfcwd\nftxt\nftxt\nf0\nf1\nf2\nf3\nfmem\nfrtd\n"
	if got := parseLsofFDCount(out); got != 4 {
		t.Fatalf("parseLsofFDCount = %d, want 4", got)
	}
	if parseLsofFDCount("") != 0 {
		t.Fatal("empty input should be 0")
	}
}
