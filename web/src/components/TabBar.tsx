import type { Session } from '../lib/types';
import type { TabsState } from '../lib/tabs';
import BusyIdleBadge from './BusyIdleBadge';

// TabBar shows the two fixed tabs (Overview, Cockpit) plus one closeable tab per
// pinned agent. Unknown/ended agent ids are skipped (prune handles removal).
export default function TabBar({ state, sessions, onActivate, onClose }: {
  state: TabsState;
  sessions: Session[];
  onActivate: (id: string) => void;
  onClose: (id: string) => void;
}) {
  const byId = new Map(sessions.map((s) => [s.id, s]));
  const cls = (id: string) => `tab${state.active === id ? ' on' : ''}`;
  return (
    <nav className="tabbar">
      <button className={cls('overview')} onClick={() => onActivate('overview')}>Overview</button>
      <button className={cls('cockpit')} onClick={() => onActivate('cockpit')}>⊞ Cockpit</button>
      <button className={cls('pipelines')} onClick={() => onActivate('pipelines')}>⛓ Pipelines</button>
      <button className={cls('context')} onClick={() => onActivate('context')}>🗒 Context &amp; Messages</button>
      <button className={cls('archive')} onClick={() => onActivate('archive')}>🗄 Archive</button>
      {state.pinned.map((id) => {
        const s = byId.get(id);
        if (!s) return null;
        return (
          <span key={id} className={cls(id)}>
            <button className="tab-label" onClick={() => onActivate(id)}>
              {id} <BusyIdleBadge status={s.status} exitCode={s.exit_code} />
            </button>
            <button className="tab-close" title="Close tab" onClick={() => onClose(id)}>✕</button>
          </span>
        );
      })}
    </nav>
  );
}
