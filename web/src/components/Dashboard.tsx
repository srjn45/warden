import { useEffect, useReducer, useRef, useState } from 'react';
import type { Session } from '../lib/types';
import { listSessions, subscribeSessions } from '../lib/api';
import { hasToken, clearToken, onAuthRequired } from '../lib/token';
import { tabsReducer, initialTabs, isFixedTab, type TabsState } from '../lib/tabs';
import { waitingTransitions } from '../lib/notify';
import { loadTheme, saveTheme, applyTheme, nextTheme, resolveTheme, type Theme } from '../lib/theme';
import { resolveShortcut } from '../lib/shortcuts';
import AttentionBar from './AttentionBar';
import TabBar from './TabBar';
import OverviewTab from './OverviewTab';
import CockpitTab from './CockpitTab';
import AgentTab from './AgentTab';
import NewAgentModal from './NewAgentModal';
import ShortcutsHelp from './ShortcutsHelp';
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
  const [showHelp, setShowHelp] = useState(false);
  const [filterSignal, setFilterSignal] = useState(0);
  const [notifyEnabled, setNotifyEnabled] = useState(false);
  const [tabs, dispatch] = useReducer(tabsReducer, undefined, loadTabs);
  const [authRequired, setAuthRequired] = useState(false);
  const [authNonce, setAuthNonce] = useState(0);
  const [tokenSet, setTokenSet] = useState(() => hasToken());
  const [theme, setTheme] = useState<Theme>(loadTheme);
  const [resolved, setResolved] = useState(() => resolveTheme(loadTheme()));
  const prevSessions = useRef<Session[]>([]);

  // Reflect the theme choice onto <html data-theme=…>, persist it, and track the
  // concrete light/dark it resolves to (for picking a matching wordmark). The
  // inline head script applies the stored value before paint to avoid a flash;
  // this keeps everything in sync as the user toggles.
  useEffect(() => {
    applyTheme(theme);
    saveTheme(theme);
    setResolved(resolveTheme(theme));
  }, [theme]);

  // While in 'system' mode, follow the OS preference live.
  useEffect(() => {
    if (theme !== 'system' || typeof window.matchMedia !== 'function') return;
    const mq = window.matchMedia('(prefers-color-scheme: dark)');
    const onChange = () => setResolved(mq.matches ? 'dark' : 'light');
    mq.addEventListener('change', onChange);
    return () => mq.removeEventListener('change', onChange);
  }, [theme]);

  const cycleTheme = () => setTheme((t) => nextTheme(t));

  // Manual refetch of the fleet (the `r` shortcut); SSE keeps it live otherwise.
  const refresh = () => { listSessions().then(setSessions).catch(() => { /* SSE will populate */ }); };

  // Global keyboard layer. resolveShortcut keeps the key→action mapping pure and
  // dormant while typing; here we just perform the action. Re-attaches when the
  // overlay/modal flags change so Esc closes the right thing.
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      const sc = resolveShortcut(e);
      if (!sc) return;
      switch (sc.kind) {
        case 'help': e.preventDefault(); setShowHelp((v) => !v); break;
        case 'new': e.preventDefault(); setShowCreate(true); break;
        case 'filter':
          e.preventDefault();
          dispatch({ kind: 'activate', id: 'overview' });
          setFilterSignal((n) => n + 1);
          break;
        case 'refresh': e.preventDefault(); refresh(); break;
        case 'nav': e.preventDefault(); dispatch({ kind: 'nav', delta: sc.delta }); break;
        case 'tab': e.preventDefault(); dispatch({ kind: 'index', index: sc.index }); break;
        case 'close':
          if (showHelp) setShowHelp(false);
          else if (showCreate) setShowCreate(false);
          else if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
          break;
      }
    }
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [showHelp, showCreate]);

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
        theme={theme}
        resolvedTheme={resolved}
        onCycleTheme={cycleTheme}
        onShowHelp={() => setShowHelp(true)}
      />
      <TabBar
        state={tabs}
        sessions={sessions}
        onActivate={(id) => dispatch({ kind: 'activate', id })}
        onClose={(id) => dispatch({ kind: 'close', id })}
      />
      <main className="tab-content">
        {tabs.active === 'overview' && <OverviewTab sessions={sessions} onSelect={select} focusSignal={filterSignal} />}
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
      {showHelp && <ShortcutsHelp onClose={() => setShowHelp(false)} />}
      {authRequired && <TokenModal onSaved={onTokenSaved} />}
    </div>
  );
}
