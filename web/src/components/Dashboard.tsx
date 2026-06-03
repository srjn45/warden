import { useEffect, useReducer, useRef, useState } from 'react';
import type { Session } from '../lib/types';
import { listSessions, subscribeSessions } from '../lib/api';
import { tabsReducer, initialTabs, type TabsState } from '../lib/tabs';
import { waitingTransitions } from '../lib/notify';
import AttentionBar from './AttentionBar';
import TabBar from './TabBar';
import OverviewTab from './OverviewTab';
import CockpitTab from './CockpitTab';
import AgentTab from './AgentTab';
import NewAgentModal from './NewAgentModal';

const TABS_KEY = 'agentctl.tabs';

function loadTabs(): TabsState {
  try {
    const raw = localStorage.getItem(TABS_KEY);
    if (raw) return JSON.parse(raw) as TabsState;
  } catch { /* corrupt / unavailable storage */ }
  return initialTabs;
}

export default function Dashboard() {
  const [sessions, setSessions] = useState<Session[]>([]);
  const [connected, setConnected] = useState(false);
  const [showCreate, setShowCreate] = useState(false);
  const [notifyEnabled, setNotifyEnabled] = useState(false);
  const [tabs, dispatch] = useReducer(tabsReducer, undefined, loadTabs);
  const prevSessions = useRef<Session[]>([]);

  // Live session list over SSE.
  useEffect(() => {
    listSessions().then(setSessions).catch(() => { /* SSE will populate */ });
    const unsub = subscribeSessions(setSessions, () => setConnected(false), () => setConnected(true));
    return unsub;
  }, []);

  // Persist tabs; prune pins for agents that ended.
  useEffect(() => { try { localStorage.setItem(TABS_KEY, JSON.stringify(tabs)); } catch { /* ignore */ } }, [tabs]);
  useEffect(() => {
    dispatch({ kind: 'prune', alive: sessions.map((s) => s.id) });
  }, [sessions]);

  // Notify when an agent newly needs input, but only while the tab is hidden.
  useEffect(() => {
    const prev = prevSessions.current;
    prevSessions.current = sessions;
    if (!notifyEnabled || Notification.permission !== 'granted') return;
    if (!document.hidden) return;
    for (const s of waitingTransitions(prev, sessions)) {
      const n = new Notification(`${s.id} needs your input`, {
        body: s.subject || s.prompt || 'Waiting for input',
        tag: s.id,
      });
      n.onclick = () => { window.focus(); dispatch({ kind: 'open', id: s.id }); n.close(); };
    }
  }, [sessions, notifyEnabled]);

  async function toggleNotify() {
    if (notifyEnabled) { setNotifyEnabled(false); return; }
    const perm = Notification.permission === 'granted'
      ? 'granted'
      : await Notification.requestPermission();
    setNotifyEnabled(perm === 'granted');
  }

  const attentionCount = sessions.filter(
    (s) => s.status === 'waiting_for_input' || s.status === 'errored' || s.status === 'orphaned',
  ).length;
  const select = (id: string) => dispatch({ kind: 'open', id });
  const activeSession = sessions.find((s) => s.id === tabs.active) ?? null;

  return (
    <div className="layout">
      <AttentionBar
        connected={connected}
        attentionCount={attentionCount}
        notifyEnabled={notifyEnabled}
        onToggleNotify={toggleNotify}
        onNew={() => setShowCreate(true)}
        onJumpAttention={() => dispatch({ kind: 'activate', id: 'overview' })}
      />
      <TabBar
        state={tabs}
        sessions={sessions}
        onActivate={(id) => dispatch({ kind: 'activate', id })}
        onClose={(id) => dispatch({ kind: 'close', id })}
      />
      <main className="tab-content">
        {tabs.active === 'overview' && <OverviewTab sessions={sessions} onSelect={select} />}
        {tabs.active === 'cockpit' && <CockpitTab sessions={sessions} onSelect={select} />}
        {activeSession && <AgentTab session={activeSession} onClosed={() => dispatch({ kind: 'close', id: activeSession.id })} />}
        {tabs.active !== 'overview' && tabs.active !== 'cockpit' && !activeSession && (
          <div className="detail empty">That agent has ended.</div>
        )}
      </main>
      {showCreate && (
        <NewAgentModal
          onClose={() => setShowCreate(false)}
          onCreated={(id) => { setShowCreate(false); dispatch({ kind: 'open', id }); }}
        />
      )}
    </div>
  );
}
