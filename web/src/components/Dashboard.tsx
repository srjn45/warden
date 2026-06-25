import { useEffect, useReducer, useRef, useState } from 'react';
import type { Session } from '../lib/types';
import { listSessions, subscribeSessions } from '../lib/api';
import { hasToken, clearToken, onAuthRequired } from '../lib/token';
import { tabsReducer, initialTabs, isFixedTab, type TabsState } from '../lib/tabs';
import { waitingTransitions } from '../lib/notify';
import AttentionBar from './AttentionBar';
import TabBar from './TabBar';
import OverviewTab from './OverviewTab';
import CockpitTab from './CockpitTab';
import AgentTab from './AgentTab';
import NewAgentModal from './NewAgentModal';
import TokenModal from './TokenModal';
import PipelinesTab from './PipelinesTab';
import ContextMessagesTab from './ContextMessagesTab';
import ArchiveTab from './ArchiveTab';

const TABS_KEY = 'warden.tabs';

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
  const [authRequired, setAuthRequired] = useState(false);
  const [authNonce, setAuthNonce] = useState(0);
  const [tokenSet, setTokenSet] = useState(() => hasToken());
  const prevSessions = useRef<Session[]>([]);

  // A 401 from any REST call surfaces the token-entry modal.
  useEffect(() => onAuthRequired(() => setAuthRequired(true)), []);

  // Live session list over SSE. Re-runs after a new token is saved (authNonce)
  // so the stream and the initial load reconnect with the new credential.
  useEffect(() => {
    listSessions().then(setSessions).catch(() => { /* SSE will populate */ });
    const unsub = subscribeSessions(setSessions, () => setConnected(false), () => setConnected(true));
    return unsub;
  }, [authNonce]);

  function onTokenSaved() {
    setAuthRequired(false);
    setTokenSet(true);
    setAuthNonce((n) => n + 1); // reconnect REST + SSE with the new token
  }

  function onClearToken() {
    clearToken();
    setTokenSet(false);
    setAuthRequired(true); // force re-entry
  }

  // Persist tabs; prune pins for agents that ended.
  useEffect(() => { try { localStorage.setItem(TABS_KEY, JSON.stringify(tabs)); } catch { /* ignore */ } }, [tabs]);
  useEffect(() => {
    dispatch({ kind: 'prune', alive: sessions.map((s) => s.id) });
  }, [sessions]);

  // Notify when an agent newly needs input, but only while the tab is hidden.
  useEffect(() => {
    const prev = prevSessions.current;
    prevSessions.current = sessions;
    if (typeof Notification === 'undefined') return; // browser without the API
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
    if (typeof Notification === 'undefined') return; // browser without the API
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
        tokenSet={tokenSet}
        onClearToken={onClearToken}
      />
      <TabBar
        state={tabs}
        sessions={sessions}
        onActivate={(id) => dispatch({ kind: 'activate', id })}
        onClose={(id) => dispatch({ kind: 'close', id })}
      />
      <main className="tab-content">
        {tabs.active === 'overview' && <OverviewTab sessions={sessions} onSelect={select} />}
        {tabs.active === 'cockpit' && <CockpitTab sessions={sessions} onSelect={select} onCreated={(id) => dispatch({ kind: 'open', id })} />}
        {tabs.active === 'pipelines' && <PipelinesTab onSelect={select} />}
        {tabs.active === 'context' && <ContextMessagesTab />}
        {tabs.active === 'archive' && <ArchiveTab />}
        {activeSession && <AgentTab session={activeSession} onClosed={() => dispatch({ kind: 'close', id: activeSession.id })} />}
        {!isFixedTab(tabs.active) && !activeSession && (
          <div className="detail empty">That agent has ended.</div>
        )}
      </main>
      {showCreate && (
        <NewAgentModal
          onClose={() => setShowCreate(false)}
          onCreated={(id) => { setShowCreate(false); dispatch({ kind: 'open', id }); }}
        />
      )}
      {authRequired && <TokenModal onSaved={onTokenSaved} />}
    </div>
  );
}
