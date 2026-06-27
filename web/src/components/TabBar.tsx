import type { Session } from '../lib/types';
import type { Route } from '../lib/router';
import { routeToPath, navigate } from '../lib/router';
import BusyIdleBadge from './BusyIdleBadge';

// TabBar shows the fixed tabs (Cockpit, Pipelines, Metrics, Archive, Others)
// plus one closeable tab per pinned agent. Tabs are real <a href> links — so
// middle-click / open-in-new-tab work — but plain left-clicks are intercepted to
// navigate client-side (pushState) instead of triggering a full page load. The
// active tab is highlighted by matching the current route's path.
//
// Context & Messages is no longer a tab — it lives behind a header button
// (AttentionBar) as a dismissible overlay.

interface FixedTab { route: Route; label: string; }
const FIXED: FixedTab[] = [
  { route: { kind: 'cockpit' }, label: '⊞ Cockpit' },
  { route: { kind: 'tui' }, label: '▢ TUI' },
  { route: { kind: 'pipelines' }, label: '⛓ Pipelines' },
  { route: { kind: 'metrics' }, label: '📊 Metrics' },
  { route: { kind: 'archive' }, label: '🗄 Archive' },
  { route: { kind: 'others' }, label: '▦ Others' },
];

export default function TabBar({ route, pinned, sessions, onClose }: {
  route: Route;
  pinned: string[];      // agent ids, in open order
  sessions: Session[];
  onClose: (id: string) => void;
}) {
  const byId = new Map(sessions.map((s) => [s.id, s]));
  const activePath = routeToPath(route);

  // Plain left-clicks navigate in-app; modified clicks (new tab / window) fall
  // through to the browser's native anchor handling.
  const onNav = (e: React.MouseEvent, to: Route) => {
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    navigate(to);
  };

  const cls = (path: string) => `tab${activePath === path ? ' on' : ''}`;

  return (
    <nav className="tabbar">
      {FIXED.map(({ route: r, label }) => {
        const path = routeToPath(r);
        return (
          <a key={path} className={cls(path)} href={path} onClick={(e) => onNav(e, r)}>
            {label}
          </a>
        );
      })}
      {pinned.map((id) => {
        const s = byId.get(id);
        if (!s) return null;
        const r: Route = { kind: 'agent', id };
        const path = routeToPath(r);
        return (
          <span key={id} className={cls(path)}>
            <a className="tab-label" href={path} onClick={(e) => onNav(e, r)}>
              {id} <BusyIdleBadge status={s.status} exitCode={s.exit_code} />
            </a>
            <button className="tab-close" title="Close tab" onClick={() => onClose(id)}>✕</button>
          </span>
        );
      })}
    </nav>
  );
}
