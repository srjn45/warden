import { useEffect, useState } from 'react';
import uPlot from 'uplot';
import 'uplot/dist/uPlot.min.css';
import { ApiError, getMetricsHistory, getSavings } from '../lib/api';
import type { MetricsSample } from '../lib/metrics';
import type { Summary } from '../lib/savings';
import {
  cpuSeries, rssSeries, totalCpuSeries, totalRssSeries,
  fleetSizeSeries, contextSeries, tokensSavedSeries, featureStackSeries,
  type AgentSeries, type TotalSeries, type ContextPoint,
} from '../lib/metricsSeries';
import { axis, agentColor, contextStateColor, featureColor, useUplot } from '../lib/uplot';
import ResourcesPanel from './ResourcesPanel';

// MetricsTab is a responsive grid of self-contained uPlot chart cards (spec
// §4.4): per-agent CPU / memory each paired with their fleet-wide total, then
// per-agent context, fleet size, and tokens saved, plus the live footprint table
// folded over from the old Overview ResourcesPanel. The grid is two columns on
// wide screens (so each per-agent chart sits beside its total) and a single
// column on narrow/mobile screens. It polls /metrics/history on the existing 5s
// cadence and /savings on a slower 30s cadence; the context series is
// client-accumulated above the tab and passed in (contextHistory) so it survives
// tab switches.
// SAVINGS_WINDOWS are the selectable trend windows. Short windows bucket by hour
// (rich intraday detail — the fix for a one-day-old ledger plotting as a single
// point); longer windows bucket by day. since is an ISO timestamp the daemon
// accepts, or undefined for all-time.
const SAVINGS_WINDOWS = [
  { key: '24h', label: '24h', bucket: 'hour' as const, ms: 24 * 3600_000 },
  { key: '48h', label: '48h', bucket: 'hour' as const, ms: 48 * 3600_000 },
  { key: '7d', label: '7d', bucket: 'day' as const, ms: 7 * 86400_000 },
  { key: '30d', label: '30d', bucket: 'day' as const, ms: 30 * 86400_000 },
  { key: 'all', label: 'All', bucket: 'day' as const, ms: 0 },
];
type SavingsWindow = (typeof SAVINGS_WINDOWS)[number];

export default function MetricsTab({ contextHistory }: { contextHistory: ContextPoint[] }) {
  const [history, setHistory] = useState<MetricsSample[]>([]);
  const [savings, setSavings] = useState<Summary | null>(null);
  const [savingsErr, setSavingsErr] = useState<ApiError | null>(null);
  // Default to 24h/hourly so the trend has points on day one. The window the
  // user picks drives both the since-window and the bucket granularity.
  const [savingsWin, setSavingsWin] = useState<SavingsWindow>(SAVINGS_WINDOWS[0]);

  // Metrics history every 5s (matches the rest of the app).
  useEffect(() => {
    let alive = true;
    const tick = () => { getMetricsHistory().then((h) => { if (alive) setHistory(h); }).catch(() => {}); };
    tick();
    const h = setInterval(tick, 5000);
    return () => { alive = false; clearInterval(h); };
  }, []);

  // Savings every 30s (slow-moving), re-fetched when the window changes. A 403
  // means the ledger is disabled — keep it so the card can show an enable hint
  // instead of an empty chart.
  useEffect(() => {
    let alive = true;
    const tick = () => {
      const since = savingsWin.ms ? new Date(Date.now() - savingsWin.ms).toISOString() : undefined;
      getSavings(since, savingsWin.bucket)
        .then((s) => { if (alive) { setSavings(s); setSavingsErr(null); } })
        .catch((e) => { if (alive && e instanceof ApiError) setSavingsErr(e); });
    };
    tick();
    const h = setInterval(tick, 30000);
    return () => { alive = false; clearInterval(h); };
  }, [savingsWin]);

  return (
    <div className="metrics">
      <MultiSeriesCard title="CPU per agent" unit="%" series={cpuSeries(history)} />
      <TotalSeriesCard title="Total CPU" unit="%" series={totalCpuSeries(history)} color="#4ea1ff" />
      <MultiSeriesCard title="Memory per agent" unit="GiB" series={rssSeries(history)} digits={2} />
      <TotalSeriesCard title="Total memory" unit="GiB" series={totalRssSeries(history)} color="#d2a8ff" digits={2} />
      <ContextCard points={contextHistory} />
      <FleetSizeCard history={history} />
      <SavingsCard
        summary={savings}
        err={savingsErr}
        win={savingsWin}
        onWin={setSavingsWin}
      />
      <SavingsBreakdownCard summary={savings} err={savingsErr} />
      <ResourcesCard />
    </div>
  );
}

// EmptyCard renders a titled card with an inline message instead of a blank
// canvas (empty / auth / disabled states).
function EmptyCard({ title, msg }: { title: string; msg: string }) {
  return (
    <section className="card metrics-card">
      <h3>{title}</h3>
      <p className="metrics-empty muted">{msg}</p>
    </section>
  );
}

// MultiSeriesCard draws one line per agent over a shared time axis (CPU, RSS).
function MultiSeriesCard({ title, unit, series, digits }: {
  title: string;
  unit: string;
  series: AgentSeries;
  digits?: number;
}) {
  const data: uPlot.AlignedData = [series.t, ...series.series.map((s) => s.values)];
  const ref = useUplot({
    sig: `${series.series.map((s) => s.id).join('|')}#${series.series.length}`,
    data,
    options: (width) => ({
      width,
      height: 200,
      scales: { x: { time: true } },
      series: [
        {},
        ...series.series.map((s) => ({
          label: s.id,
          stroke: agentColor(s.id),
          width: 1.5,
          points: { show: false },
          value: (_u: uPlot, v: number | null) => (v == null ? '—' : v.toFixed(digits ?? 1)),
        })),
      ],
      axes: [axis(), axis({ label: unit })],
    }),
  });
  if (series.t.length === 0 || series.series.length === 0) {
    return <EmptyCard title={title} msg="No samples yet." />;
  }
  return (
    <section className="card metrics-card">
      <h3>{title}</h3>
      <div ref={ref} className="metrics-chart" />
    </section>
  );
}

// TotalSeriesCard draws a single fleet-wide aggregate line (Total CPU / memory),
// meant to sit beside its per-agent breakdown card in the grid.
function TotalSeriesCard({ title, unit, series, color, digits }: {
  title: string;
  unit: string;
  series: TotalSeries;
  color: string;
  digits?: number;
}) {
  const data: uPlot.AlignedData = [series.t, series.values];
  const ref = useUplot({
    sig: `total-${title}`,
    data,
    options: (width) => ({
      width,
      height: 200,
      scales: { x: { time: true } },
      series: [
        {},
        {
          label: `total ${unit}`,
          stroke: color,
          fill: `${color}22`,
          width: 2,
          points: { show: false },
          value: (_u: uPlot, v: number | null) => (v == null ? '—' : v.toFixed(digits ?? 1)),
        },
      ],
      axes: [axis(), axis({ label: unit })],
    }),
  });
  if (series.t.length === 0) {
    return <EmptyCard title={title} msg="No samples yet." />;
  }
  return (
    <section className="card metrics-card">
      <h3>{title}</h3>
      <div ref={ref} className="metrics-chart" />
    </section>
  );
}

// ContextCard draws the client-accumulated per-agent context-token series and a
// legend whose dot is colored by each agent's latest context_state, so pressure
// (ok/warning/critical) is visible at a glance.
function ContextCard({ points }: { points: ContextPoint[] }) {
  const cs = contextSeries(points);
  const data: uPlot.AlignedData = [cs.t, ...cs.series.map((s) => s.values)];
  const ref = useUplot({
    sig: `${cs.series.map((s) => s.id).join('|')}#${cs.series.length}`,
    data,
    options: (width) => ({
      width,
      height: 200,
      scales: { x: { time: true } },
      legend: { show: false },
      series: [
        {},
        ...cs.series.map((s) => ({
          label: s.id,
          stroke: agentColor(s.id),
          width: 1.5,
          points: { show: false },
        })),
      ],
      axes: [axis(), axis({ label: 'tokens' })],
    }),
  });
  if (cs.t.length === 0 || cs.series.length === 0) {
    return <EmptyCard title="Context per agent" msg="Accumulating context history… (resets on reload)" />;
  }
  return (
    <section className="card metrics-card">
      <h3>Context per agent</h3>
      <div ref={ref} className="metrics-chart" />
      <div className="metrics-legend">
        {cs.series.map((s) => (
          <span key={s.id} className="metrics-legend-item">
            <span className="metrics-dot" style={{ background: contextStateColor(cs.stateById[s.id] ?? '') }} />
            {s.id}
          </span>
        ))}
      </div>
    </section>
  );
}

// FleetSizeCard draws the single-series number-of-agents trend.
function FleetSizeCard({ history }: { history: MetricsSample[] }) {
  const fs = fleetSizeSeries(history);
  const data: uPlot.AlignedData = [fs.t, fs.count];
  const ref = useUplot({
    sig: 'fleet-size',
    data,
    options: (width) => ({
      width,
      height: 160,
      scales: { x: { time: true } },
      series: [{}, { label: 'agents', stroke: '#4ea1ff', width: 2, points: { show: false } }],
      axes: [axis(), axis({ label: 'agents' })],
    }),
  });
  if (fs.t.length === 0) return <EmptyCard title="Number of agents" msg="No samples yet." />;
  return (
    <section className="card metrics-card">
      <h3>Number of agents</h3>
      <div ref={ref} className="metrics-chart" />
    </section>
  );
}

// SavingsCard shows the headline saved tokens/dollars, a window selector, and a
// dual-axis trend: per-bucket saved tokens (filled area, left) plus the running
// cumulative total (line, right). The bucket width follows the chosen window
// (hourly for ≤48h, daily beyond), so a fresh ledger plots a real curve instead
// of a single point. On 403 (savings disabled) it renders the enable hint.
function SavingsCard({ summary, err, win, onWin }: {
  summary: Summary | null;
  err: ApiError | null;
  win: SavingsWindow;
  onWin: (w: SavingsWindow) => void;
}) {
  const ss = tokensSavedSeries(summary?.buckets);
  const data: uPlot.AlignedData = [ss.x, ss.saved, ss.cumulative];
  const unit = win.bucket === 'hour' ? 'tokens/hr' : 'tokens/day';
  const ref = useUplot({
    sig: `tokens-saved-${win.key}`,
    data,
    options: (width) => ({
      width,
      height: 180,
      scales: { x: { time: true }, cum: {} },
      series: [
        {},
        {
          label: 'saved',
          stroke: '#3fb950',
          fill: '#3fb95033',
          width: 2,
          points: { show: false },
          value: (_u: uPlot, v: number | null) => (v == null ? '—' : v.toLocaleString()),
        },
        {
          label: 'cumulative',
          scale: 'cum',
          stroke: '#d2a8ff',
          width: 1.5,
          dash: [4, 3],
          points: { show: false },
          value: (_u: uPlot, v: number | null) => (v == null ? '—' : v.toLocaleString()),
        },
      ],
      axes: [
        axis(),
        axis({ label: unit }),
        axis({ side: 1, scale: 'cum', label: 'cumulative', grid: { show: false } }),
      ],
    }),
  });
  if (err?.status === 403) {
    return (
      <EmptyCard
        title="Tokens saved"
        msg="Savings ledger disabled. Set `savings: true` in the daemon config to track token reductions."
      />
    );
  }
  return (
    <section className="card metrics-card">
      <div className="metrics-card-head">
        <h3>Tokens saved</h3>
        <WindowPicker win={win} onWin={onWin} />
      </div>
      {summary && (
        <p className="metrics-headline">
          <strong>{summary.saved_tokens.toLocaleString()}</strong> tokens ·{' '}
          <strong>${summary.saved_dollars.toFixed(2)}</strong> saved
        </p>
      )}
      {ss.x.length === 0
        ? <p className="metrics-empty muted">No savings recorded in this window.</p>
        : <div ref={ref} className="metrics-chart" />}
    </section>
  );
}

// WindowPicker is the segmented control selecting the savings trend window (and,
// implicitly, the bucket granularity).
function WindowPicker({ win, onWin }: { win: SavingsWindow; onWin: (w: SavingsWindow) => void }) {
  return (
    <div className="metrics-seg" role="group" aria-label="trend window">
      {SAVINGS_WINDOWS.map((w) => (
        <button
          key={w.key}
          type="button"
          className={`metrics-seg-btn${w.key === win.key ? ' is-active' : ''}`}
          aria-pressed={w.key === win.key}
          onClick={() => onWin(w)}
        >
          {w.label}
        </button>
      ))}
    </div>
  );
}

// SavingsBreakdownCard draws the per-feature stacked-area split of the saved
// tokens over the same window, so it's clear which lifecycle feature (offload,
// commit, check, compact) drives the savings. Each band is the cumulative top
// edge (see featureStackSeries); the tallest paints first so the slices read as
// a stack. Hidden until there are buckets with a per-feature split.
function SavingsBreakdownCard({ summary, err }: { summary: Summary | null; err: ApiError | null }) {
  const fs = featureStackSeries(summary?.buckets);
  // Draw the tallest band first (uPlot paints later series on top), so add the
  // features in reverse total order with each as a filled area to its top edge.
  const drawn = [...fs.features].reverse();
  const data: uPlot.AlignedData = [fs.t, ...drawn.map((f) => fs.tops[f])];
  const ref = useUplot({
    sig: `savings-breakdown-${fs.features.join('|')}`,
    data,
    options: (width) => ({
      width,
      height: 180,
      scales: { x: { time: true } },
      legend: { show: false },
      series: [
        {},
        ...drawn.map((f) => ({
          label: f,
          stroke: featureColor(f),
          fill: `${featureColor(f)}55`,
          width: 1.5,
          points: { show: false },
          value: (_u: uPlot, v: number | null) => (v == null ? '—' : v.toLocaleString()),
        })),
      ],
      axes: [axis(), axis({ label: 'tokens' })],
    }),
  });
  if (err?.status === 403) return null; // the SavingsCard already shows the enable hint
  return (
    <section className="card metrics-card">
      <h3>Savings by feature</h3>
      {fs.features.length === 0 || fs.t.length === 0
        ? <p className="metrics-empty muted">No per-feature breakdown yet.</p>
        : (
          <>
            <div ref={ref} className="metrics-chart" />
            <div className="metrics-legend">
              {fs.features.map((f) => (
                <span key={f} className="metrics-legend-item">
                  <span className="metrics-dot" style={{ background: featureColor(f) }} />
                  {f}
                </span>
              ))}
            </div>
          </>
        )}
    </section>
  );
}

// ResourcesCard folds the old Overview Resources panel (live system footprint +
// per-agent RSS/CPU table + attributed-RSS history) into the Metrics tab.
function ResourcesCard() {
  return (
    <section className="card metrics-card metrics-wide">
      <h3>Live footprint</h3>
      <ResourcesPanel />
    </section>
  );
}
