import { useEffect, useState } from 'react';
import uPlot from 'uplot';
import 'uplot/dist/uPlot.min.css';
import { ApiError, getMetricsHistory, getSavings } from '../lib/api';
import type { MetricsSample } from '../lib/metrics';
import type { Summary } from '../lib/savings';
import {
  cpuSeries, rssSeries, fleetSizeSeries, contextSeries, tokensSavedSeries,
  type AgentSeries, type ContextPoint,
} from '../lib/metricsSeries';
import { axis, agentColor, contextStateColor, useUplot } from '../lib/uplot';
import ResourcesPanel from './ResourcesPanel';

// MetricsTab is a scrollable column of self-contained uPlot chart cards (spec
// §4.4): per-agent CPU / memory / context, fleet size, and tokens saved, plus
// the live footprint table folded over from the old Overview ResourcesPanel.
// It polls /metrics/history on the existing 5s cadence and /savings on a slower
// 30s cadence; the context series is client-accumulated above the tab and
// passed in (contextHistory) so it survives tab switches.
export default function MetricsTab({ contextHistory }: { contextHistory: ContextPoint[] }) {
  const [history, setHistory] = useState<MetricsSample[]>([]);
  const [savings, setSavings] = useState<Summary | null>(null);
  const [savingsErr, setSavingsErr] = useState<ApiError | null>(null);

  // Metrics history every 5s (matches the rest of the app).
  useEffect(() => {
    let alive = true;
    const tick = () => { getMetricsHistory().then((h) => { if (alive) setHistory(h); }).catch(() => {}); };
    tick();
    const h = setInterval(tick, 5000);
    return () => { alive = false; clearInterval(h); };
  }, []);

  // Savings every 30s (slow-moving). A 403 means the ledger is disabled — keep
  // it so the card can show an enable hint instead of an empty chart.
  useEffect(() => {
    let alive = true;
    const tick = () => {
      getSavings(undefined, 'day')
        .then((s) => { if (alive) { setSavings(s); setSavingsErr(null); } })
        .catch((e) => { if (alive && e instanceof ApiError) setSavingsErr(e); });
    };
    tick();
    const h = setInterval(tick, 30000);
    return () => { alive = false; clearInterval(h); };
  }, []);

  return (
    <div className="metrics">
      <MultiSeriesCard title="CPU per agent" unit="%" series={cpuSeries(history)} />
      <MultiSeriesCard title="Memory per agent" unit="GiB" series={rssSeries(history)} digits={2} />
      <ContextCard points={contextHistory} />
      <FleetSizeCard history={history} />
      <SavingsCard summary={savings} err={savingsErr} />
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

// SavingsCard shows the headline saved tokens/dollars plus a daily-saved bar
// trend. On 403 (savings disabled) it renders the enable hint, not a chart.
function SavingsCard({ summary, err }: { summary: Summary | null; err: ApiError | null }) {
  const ss = tokensSavedSeries(summary?.buckets);
  const data: uPlot.AlignedData = [ss.x, ss.saved];
  const ref = useUplot({
    sig: 'tokens-saved',
    data,
    options: (width) => ({
      width,
      height: 160,
      scales: { x: { time: true } },
      series: [
        {},
        {
          label: 'saved tokens',
          stroke: '#3fb950',
          fill: '#3fb95033',
          width: 2,
          points: { show: false },
          value: (_u: uPlot, v: number | null) => (v == null ? '—' : v.toLocaleString()),
        },
      ],
      axes: [axis(), axis({ label: 'tokens/day' })],
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
      <h3>Tokens saved</h3>
      {summary && (
        <p className="metrics-headline">
          <strong>{summary.saved_tokens.toLocaleString()}</strong> tokens ·{' '}
          <strong>${summary.saved_dollars.toFixed(2)}</strong> saved
        </p>
      )}
      {ss.x.length === 0
        ? <p className="metrics-empty muted">No savings recorded yet.</p>
        : <div ref={ref} className="metrics-chart" />}
    </section>
  );
}

// ResourcesCard folds the old Overview Resources panel (live system footprint +
// per-agent RSS/CPU table + attributed-RSS history) into the Metrics tab.
function ResourcesCard() {
  return (
    <section className="card metrics-card">
      <h3>Live footprint</h3>
      <ResourcesPanel />
    </section>
  );
}
