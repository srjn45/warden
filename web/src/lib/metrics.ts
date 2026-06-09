// Mirrors the daemon's metrics.Sample wire shape (internal/metrics/types.go).
export interface SystemStats {
  total_bytes: number;
  used_bytes: number;
  free_bytes: number;
  wired_bytes: number;
  compressed_bytes: number;
  swap_used_bytes: number;
  pressure_level: string;
  agent_count: number;
  attributed_rss_bytes: number;
}

export interface AgentStat {
  id: string;
  status: string;
  paneable: boolean;
  rss_bytes: number;
  cpu_percent: number;
  proc_count: number;
  uptime_sec: number;
}

export interface DaemonStat {
  rss_bytes: number;
  goroutines: number;
  open_fds: number;
}

export interface MetricsSample {
  taken_at: string;
  system: SystemStats;
  agents: AgentStat[];
  daemon: DaemonStat;
}

// pressureRank maps a level name to its numeric severity (matches the Go enum:
// normal=1, warn=2, critical=4).
export function pressureRank(level: string): number {
  switch (level) {
    case 'warn': return 2;
    case 'critical': return 4;
    default: return 1;
  }
}

// HistorySeries is column-oriented data ready for a uPlot chart.
export interface HistorySeries {
  t: number[];        // unix seconds, oldest-first
  rssGiB: number[];   // attributed RSS in GiB
  pressure: number[]; // pressure rank 1/2/4
}

// historySeries converts daemon history (newest-first) into oldest-first
// parallel arrays for charting.
export function historySeries(samples: MetricsSample[]): HistorySeries {
  const ordered = [...samples].sort(
    (a, b) => new Date(a.taken_at).getTime() - new Date(b.taken_at).getTime(),
  );
  return {
    t: ordered.map((s) => new Date(s.taken_at).getTime() / 1000),
    rssGiB: ordered.map((s) => s.system.attributed_rss_bytes / 2 ** 30),
    pressure: ordered.map((s) => pressureRank(s.system.pressure_level)),
  };
}

// fmtBytes renders a byte count as a compact IEC string.
export function fmtBytes(b: number): string {
  if (b < 1024) return `${b} B`;
  const units = ['KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let v = b / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(1)} ${units[i]}`;
}
