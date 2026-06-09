import { describe, it, expect } from 'vitest';
import { historySeries, fmtBytes, type MetricsSample } from './metrics';

const sample = (t: string, rss: number, level: string): MetricsSample => ({
  taken_at: t,
  system: { total_bytes: 0, used_bytes: 0, free_bytes: 0, wired_bytes: 0, compressed_bytes: 0, swap_used_bytes: 0, pressure_level: level, agent_count: 0, attributed_rss_bytes: rss },
  agents: [],
  daemon: { rss_bytes: 0, goroutines: 0, open_fds: 0 },
});

describe('historySeries', () => {
  it('builds parallel x/rss/pressure arrays sorted oldest-first', () => {
    // input newest-first (as the daemon returns it)
    const data = [
      sample('2026-06-09T10:00:30Z', 300, 'critical'),
      sample('2026-06-09T10:00:00Z', 100, 'normal'),
    ];
    const s = historySeries(data);
    expect(s.t.length).toBe(2);
    expect(s.rssGiB[0]).toBeCloseTo(100 / 2 ** 30);
    expect(s.pressure[0]).toBe(1); // normal=1
    expect(s.pressure[1]).toBe(4); // critical=4
    expect(s.t[0]).toBeLessThan(s.t[1]); // oldest first
  });
});

describe('fmtBytes', () => {
  it('renders IEC units', () => {
    expect(fmtBytes(0)).toBe('0 B');
    expect(fmtBytes(1536)).toBe('1.5 KiB');
    expect(fmtBytes(2 * 2 ** 30)).toBe('2.0 GiB');
  });
});
