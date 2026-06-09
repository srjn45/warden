import { useEffect, useRef, useState } from 'react';
import uPlot from 'uplot';
import 'uplot/dist/uPlot.min.css';
import { getMetrics, getMetricsHistory } from '../lib/api';
import { historySeries, fmtBytes, type MetricsSample } from '../lib/metrics';

// ResourcesPanel shows warden's live footprint (system memory + per-agent RSS/CPU)
// plus an attributed-RSS history chart, so a memory ramp is visible. Web-only.
export default function ResourcesPanel() {
  const [live, setLive] = useState<MetricsSample | null>(null);
  const [history, setHistory] = useState<MetricsSample[]>([]);
  const chartRef = useRef<HTMLDivElement>(null);
  const plotRef = useRef<uPlot | null>(null);

  // Poll the live snapshot + history every 5s.
  useEffect(() => {
    let alive = true;
    const tick = () => {
      getMetrics().then((m) => { if (alive) setLive(m); }).catch(() => {});
      getMetricsHistory().then((h) => { if (alive) setHistory(h); }).catch(() => {});
    };
    tick();
    const h = setInterval(tick, 5000);
    return () => { alive = false; clearInterval(h); };
  }, []);

  // (Re)draw the uPlot chart whenever history changes.
  useEffect(() => {
    if (!chartRef.current) return;
    const s = historySeries(history);
    const data: uPlot.AlignedData = [s.t, s.rssGiB, s.pressure];
    if (!plotRef.current) {
      const opts: uPlot.Options = {
        width: chartRef.current.clientWidth || 600,
        height: 160,
        scales: { x: { time: true } },
        series: [
          {},
          { label: 'attributed RSS (GiB)', stroke: '#4ea1ff', width: 2 },
          { label: 'pressure', stroke: '#ff6b6b', width: 1, scale: 'p' },
        ],
        axes: [{}, { label: 'GiB' }, { scale: 'p', side: 1, label: 'pressure' }],
      };
      plotRef.current = new uPlot(opts, data, chartRef.current);
    } else {
      plotRef.current.setData(data);
    }
  }, [history]);

  // Tear down the plot on unmount.
  useEffect(() => () => { plotRef.current?.destroy(); plotRef.current = null; }, []);

  const agents = [...(live?.agents ?? [])].sort((a, b) => b.rss_bytes - a.rss_bytes);

  return (
    <div className="resources">
      {live && (
        <div className="resources-summary muted">
          {fmtBytes(live.system.used_bytes)} / {fmtBytes(live.system.total_bytes)} used ·{' '}
          {fmtBytes(live.system.swap_used_bytes)} swap · pressure {live.system.pressure_level} ·{' '}
          {fmtBytes(live.system.attributed_rss_bytes)} attributed across {live.system.agent_count} agents
        </div>
      )}
      <div ref={chartRef} className="resources-chart" />
      <table className="resources-table">
        <thead>
          <tr><th>agent</th><th>RSS</th><th>CPU%</th><th>procs</th></tr>
        </thead>
        <tbody>
          {agents.map((a) => (
            <tr key={a.id}>
              <td>{a.id}</td>
              <td>{a.paneable ? fmtBytes(a.rss_bytes) : '—'}</td>
              <td>{a.cpu_percent.toFixed(1)}</td>
              <td>{a.proc_count}</td>
            </tr>
          ))}
          {agents.length === 0 && <tr><td colSpan={4} className="muted">no live agents</td></tr>}
        </tbody>
      </table>
    </div>
  );
}
