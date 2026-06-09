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
