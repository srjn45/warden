// Shared uPlot lifecycle for the Metrics tab's chart cards. Factors the
// create/update/destroy dance (and the theme-neutral axis styling first proven
// in ResourcesPanel) out of the five charts so each card is just "here are my
// options and data". useUplot recreates the plot when the SERIES STRUCTURE
// changes (its `sig`) — the key to surviving agent churn, since uPlot can't add
// or drop a series after construction — and otherwise streams new data in with
// setData on every poll.
import { useEffect, useRef } from 'react';
import uPlot from 'uplot';

// Theme-neutral grays: uPlot defaults axis text/ticks/grid to black, which
// vanishes on the dark theme. #888 reads on both light and dark (matches
// ResourcesPanel).
export const AXIS_STROKE = '#888';
export const GRID_STROKE = '#8883';

// axis builds a theme-neutral uPlot axis, optionally overridden (e.g. a label
// or a secondary scale).
export function axis(extra: Partial<uPlot.Axis> = {}): uPlot.Axis {
  return {
    stroke: AXIS_STROKE,
    grid: { stroke: GRID_STROKE },
    ticks: { stroke: GRID_STROKE },
    ...extra,
  };
}

// agentColor maps an agent id to a stable hue, so an agent keeps its color
// across charts and across re-renders (deterministic hash → HSL).
export function agentColor(id: string): string {
  let h = 0;
  for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) >>> 0;
  return `hsl(${h % 360} 70% 55%)`;
}

// contextStateColor maps a context_state to a pressure color for the legend.
export function contextStateColor(state: string): string {
  switch (state) {
    case 'critical': return '#ff6b6b';
    case 'warning': return '#e0a000';
    case 'ok': return '#3fb950';
    default: return AXIS_STROKE;
  }
}

export interface UplotSpec {
  // sig identifies the series STRUCTURE. When it changes the plot is destroyed
  // and rebuilt (uPlot can't mutate its series set); when it's stable, only the
  // data is streamed in. Build it from the ordered series ids.
  sig: string;
  // options is called only on (re)build, with the live container width.
  options: (width: number) => uPlot.Options;
  data: uPlot.AlignedData;
}

// useUplot manages one chart's lifecycle and returns the ref to attach to its
// container <div>. It rebuilds on `sig` change, streams data otherwise, follows
// container width on resize, and tears the plot down on unmount.
export function useUplot(spec: UplotSpec) {
  const elRef = useRef<HTMLDivElement>(null);
  const plotRef = useRef<uPlot | null>(null);
  const sigRef = useRef<string | null>(null);

  useEffect(() => {
    const el = elRef.current;
    if (!el) return;
    if (!plotRef.current || sigRef.current !== spec.sig) {
      plotRef.current?.destroy();
      plotRef.current = new uPlot(spec.options(el.clientWidth || 600), spec.data, el);
      sigRef.current = spec.sig;
    } else {
      plotRef.current.setData(spec.data);
    }
  }, [spec.sig, spec.data]);

  // Keep the plot the width of its (responsive) container.
  useEffect(() => {
    const el = elRef.current;
    if (!el || typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver(() => {
      const p = plotRef.current;
      if (p && el.clientWidth) p.setSize({ width: el.clientWidth, height: p.height });
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  useEffect(() => () => { plotRef.current?.destroy(); plotRef.current = null; }, []);

  return elRef;
}
