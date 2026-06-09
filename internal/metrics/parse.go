package metrics

import (
	"strconv"
	"strings"
)

// ProcRow is one row of the ps table.
type ProcRow struct {
	PID      int
	PPID     int
	RSSKiB   uint64
	CPU      float64
	EtimeSec int64
}

// parseEtime parses BSD ps elapsed time: "[[dd-]hh:]mm:ss" → seconds.
func parseEtime(s string) int64 {
	s = strings.TrimSpace(s)
	var days int64
	if i := strings.IndexByte(s, '-'); i >= 0 {
		days, _ = strconv.ParseInt(s[:i], 10, 64)
		s = s[i+1:]
	}
	parts := strings.Split(s, ":")
	var h, m, sec int64
	switch len(parts) {
	case 3:
		h, _ = strconv.ParseInt(parts[0], 10, 64)
		m, _ = strconv.ParseInt(parts[1], 10, 64)
		sec, _ = strconv.ParseInt(parts[2], 10, 64)
	case 2:
		m, _ = strconv.ParseInt(parts[0], 10, 64)
		sec, _ = strconv.ParseInt(parts[1], 10, 64)
	default:
		return 0
	}
	return days*86400 + h*3600 + m*60 + sec
}

// parsePSTable parses `ps -axo pid=,ppid=,rss=,pcpu=,etime=` output (5
// whitespace-separated columns; etime has no internal spaces) into a pid→row
// map. Malformed rows are skipped.
func parsePSTable(raw string) map[int]ProcRow {
	out := make(map[int]ProcRow)
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Fields(line)
		if len(f) != 5 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		rss, _ := strconv.ParseUint(f[2], 10, 64)
		cpu, _ := strconv.ParseFloat(f[3], 64)
		out[pid] = ProcRow{PID: pid, PPID: ppid, RSSKiB: rss, CPU: cpu, EtimeSec: parseEtime(f[4])}
	}
	return out
}

// aggregateTree sums RSS (→ bytes), CPU%, process count, and the oldest root's
// uptime over each root pid and all its descendants. Roots absent from the table
// contribute nothing. RSS is returned in BYTES (ps reports KiB).
func aggregateTree(tbl map[int]ProcRow, roots []int) (rssBytes uint64, cpu float64, procs int, uptimeSec int64) {
	children := make(map[int][]int, len(tbl))
	for pid, r := range tbl {
		children[r.PPID] = append(children[r.PPID], pid)
	}
	visited := make(map[int]bool)
	var walk func(pid int)
	walk = func(pid int) {
		if visited[pid] {
			return
		}
		r, ok := tbl[pid]
		if !ok {
			return
		}
		visited[pid] = true
		rssBytes += r.RSSKiB * 1024
		cpu += r.CPU
		procs++
		for _, c := range children[pid] {
			walk(c)
		}
	}
	for _, root := range roots {
		if r, ok := tbl[root]; ok && r.EtimeSec > uptimeSec {
			uptimeSec = r.EtimeSec
		}
		walk(root)
	}
	return rssBytes, cpu, procs, uptimeSec
}

// parseVMStat parses `vm_stat` output: the page size from the header and each
// "Key: N." line into a counts map (keyed by the text before the colon).
func parseVMStat(raw string) (pageSize int64, counts map[string]int64) {
	counts = make(map[string]int64)
	pageSize = 4096 // sane default if the header is missing
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Mach Virtual Memory Statistics") {
			if i := strings.Index(line, "page size of "); i >= 0 {
				rest := line[i+len("page size of "):]
				rest = strings.TrimSuffix(strings.TrimSpace(rest), " bytes)")
				if ff := strings.Fields(rest); len(ff) > 0 {
					if n, err := strconv.ParseInt(ff[0], 10, 64); err == nil {
						pageSize = n
					}
				}
			}
			continue
		}
		i := strings.LastIndex(line, ":")
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line[i+1:]), "."))
		if n, err := strconv.ParseInt(val, 10, 64); err == nil {
			counts[key] = n
		}
	}
	return pageSize, counts
}

// parseSwapUsed extracts the "used = N.NNM" figure from `sysctl vm.swapusage`
// and returns it in bytes (suffix M=MiB, G=GiB, K=KiB).
func parseSwapUsed(raw string) uint64 {
	i := strings.Index(raw, "used =")
	if i < 0 {
		return 0
	}
	f := strings.Fields(raw[i+len("used ="):])
	if len(f) == 0 {
		return 0
	}
	tok := f[0]
	mult := 1.0
	switch {
	case strings.HasSuffix(tok, "G"):
		mult, tok = 1<<30, strings.TrimSuffix(tok, "G")
	case strings.HasSuffix(tok, "M"):
		mult, tok = 1<<20, strings.TrimSuffix(tok, "M")
	case strings.HasSuffix(tok, "K"):
		mult, tok = 1<<10, strings.TrimSuffix(tok, "K")
	}
	v, err := strconv.ParseFloat(tok, 64)
	if err != nil {
		return 0
	}
	return uint64(v * mult)
}

// parseMemSize parses `sysctl -n hw.memsize` (bare value or "hw.memsize: N").
func parseMemSize(raw string) uint64 {
	s := strings.TrimSpace(raw)
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = strings.TrimSpace(s[i+1:])
	}
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseUint(f[0], 10, 64)
	return v
}

// buildSystemStats assembles SystemStats from parsed pieces. UsedBytes is
// total-free (guarded against underflow). Agent count/attributed RSS are filled
// by the Collector, not here.
func buildSystemStats(pageSize int64, counts map[string]int64, total, swapUsed uint64, pressure string) SystemStats {
	px := uint64(pageSize)
	free := uint64(counts["Pages free"]) * px
	wired := uint64(counts["Pages wired down"]) * px
	compressed := uint64(counts["Pages occupied by compressor"]) * px
	used := uint64(0)
	if total > free {
		used = total - free
	}
	return SystemStats{
		TotalBytes:      total,
		UsedBytes:       used,
		FreeBytes:       free,
		WiredBytes:      wired,
		CompressedBytes: compressed,
		SwapUsedBytes:   swapUsed,
		PressureLevel:   pressure,
	}
}

// parseLsofFDCount counts numbered file descriptors in `lsof -F f` output:
// lines of the form "f<digits>" (e.g. "f0", "f12"). Pseudo-entries (fcwd, ftxt,
// fmem, frtd) and the "p<pid>" header line are ignored.
func parseLsofFDCount(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 2 || line[0] != 'f' {
			continue
		}
		allDigits := true
		for _, r := range line[1:] {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			n++
		}
	}
	return n
}
