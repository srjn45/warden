import { useEffect, useRef, useState } from 'react';
import type { Session } from '../lib/types';
import { listSessions, subscribeSessions } from '../lib/api';
import { hasToken, clearToken, onAuthRequired } from '../lib/token';
import {
  useRoute, navigate, redirectRootToDefault, DEFAULT_ROUTE, type Route,
} from '../lib/router';
import {
  loadPinned, savePinned, openPin, closePin, prunePins, navRoute, routeByIndex,
} from '../lib/tabs';
import { waitingTransitions } from '../lib/notify';
import { appendContextPoint, type ContextPoint } from '../lib/metricsSeries';
import { loadTheme, saveTheme, applyTheme, nextTheme, resolveTheme, type Theme } from '../lib/theme';
import { resolveShortcut } from '../lib/shortcuts';
import AttentionBar from './AttentionBar';
import TabBar from './TabBar';
import OthersTab from './OthersTab';
import CockpitTab from './CockpitTab';
import MetricsTab from './MetricsTab';
import AgentTab from './AgentTab';
import NewAgentModal from './NewAgentModal';
import ShortcutsHelp from './ShortcutsHelp';
import TokenModal from './TokenModal';
import PipelinesTab from './PipelinesTab';
import ContextOverlay from './ContextOverlay';
import ArchiveTab from './ArchiveTab';

export default function Dashboard() {
  // One-time: redirect `/` → /cockpit before the route is first read below, so
  // there is a single canonical home URL. replaceState (not push) keeps Back
  // from bouncing; this initializer runs once on mount only — guarding the loop.
  useState(redirectRootToDefault);
  const route = useRoute();

  const [sessions, setSessions] = useState<Session[]>([]);
  const [connected, setConnected] = useState(false);
  const [showCreate, setShowCreate] = useState(false);
  const [showHelp, setShowHelp] = useState(false);
  const [showContext, setShowContext] = useState(false);
  const [notifyEnabled, setNotifyEnabled] = useState(false);
  const [pinned, setPinned] = useState<string[]>(loadPinned);
  const [authRequired, setAuthRequired] = useState(false);
  const [authNonce, setAuthNonce] = useState(0);
  const [tokenSet, setTokenSet] = useState(() => hasToken());
  const [theme, setTheme] = useState<Theme>(loadTheme);
  const [resolved, setResolved] = useState(() => resolveTheme(loadTheme()));
  const prevSessions = useRef<Session[]>([]);
  // Client-accumulated context-token history feeding the Metrics tab's
  // Context-per-agent chart (spec §4.4 item 3). Owned here, above the tab, so it
  // survives tab switches; a full page reload starts the window fresh. Sampled
  // on a 5s timer reading the latest live sessions (kept in a ref so the timer
  // need not re-arm on every SSE push).
  const [contextHistory, setContextHistory] = useState<ContextPoint[]>([]);
  const sessionsRef = useRef<Session[]>([]);
  sessionsRef.current = sessions;

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

  // Open (pin + activate) an agent pane: add to the pinned list and navigate to
  // its URL. Closing removes the pin and, if it was the active pane, returns to
  // the cockpit (the new default home).
  const select = (id: string) => { setPinned((p) => openPin(p, id)); navigate({ kind: 'agent', id }); };
  const closeAgent = (id: string) => {
    setPinned((p) => closePin(p, id));
    if (route.kind === 'agent' && route.id === id) navigate(DEFAULT_ROUTE);
  };

  // Global keyboard layer. resolveShortcut keeps the key→action mapping pure and
  // dormant while typing; here we just perform the action. Re-attaches when the
  // overlay/modal flags or route/pinned change so navigation + Esc target the
  // right thing.
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      const sc = resolveShortcut(e);
      if (!sc) return;
      switch (sc.kind) {
        case 'help': e.preventDefault(); setShowHelp((v) => !v); break;
        case 'new': e.preventDefault(); setShowCreate(true); break;
        case 'filter':
          // The agent filter lived in the old Overview grid, which is gone; the
          // `/` shortcut now just jumps to Others (the needs-you / activity hub).
          e.preventDefault();
          navigate({ kind: 'others' });
          break;
        case 'refresh': e.preventDefault(); refresh(); break;
        case 'nav': e.preventDefault(); navigate(navRoute(pinned, route, sc.delta)); break;
        case 'tab': {
          e.preventDefault();
          const r = routeByIndex(pinned, sc.index);
          if (r) navigate(r);
          break;
        }
        case 'close':
          if (showHelp) setShowHelp(false);
          else if (showContext) setShowContext(false);
          else if (showCreate) setShowCreate(false);
          else if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
          break;
      }
    }
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [showHelp, showCreate, showContext, route, pinned]);

  // Sample each agent's live context fill every 5s into the ring buffer.
  useEffect(() => {
    const tick = () => setContextHistory((p) => appendContextPoint(p, sessionsRef.current, Date.now() / 1000));
    tick();
    const h = setInterval(tick, 5000);
    return () => clearInterval(h);
  }, []);

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

  // Persist the pinned list (active tab is URL-driven, not stored); prune pins
  // for agents that ended.
  useEffect(() => { savePinned(pinned); }, [pinned]);
  useEffect(() => {
    setPinned((p) => prunePins(p, sessions.map((s) => s.id)));
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
      n.onclick = () => { window.focus(); select(s.id); n.close(); };
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
  const activeSession = route.kind === 'agent'
    ? sessions.find((s) => s.id === route.id) ?? null
    : null;

  return (
    <div className="layout">
      <AttentionBar
        connected={connected}
        attentionCount={attentionCount}
        notifyEnabled={notifyEnabled}
        onToggleNotify={toggleNotify}
        onNew={() => setShowCreate(true)}
        onJumpAttention={() => navigate({ kind: 'others' })}
        tokenSet={tokenSet}
        onClearToken={onClearToken}
        theme={theme}
        resolvedTheme={resolved}
        onCycleTheme={cycleTheme}
        onShowHelp={() => setShowHelp(true)}
        onToggleContext={() => setShowContext((v) => !v)}
      />
      <TabBar
        route={route}
        pinned={pinned}
        sessions={sessions}
        onClose={closeAgent}
      />
      <main className="tab-content">
        {route.kind === 'others' && <OthersTab sessions={sessions} onSelect={select} />}
        {route.kind === 'cockpit' && <CockpitTab sessions={sessions} onSelect={select} onCreated={select} />}
        {route.kind === 'pipelines' && <PipelinesTab onSelect={select} />}
        {route.kind === 'metrics' && <MetricsTab contextHistory={contextHistory} />}
        {route.kind === 'archive' && <ArchiveTab />}
        {route.kind === 'agent' && (activeSession
          ? <AgentTab session={activeSession} onClosed={() => closeAgent(activeSession.id)} />
          : <div className="detail empty">That agent has ended.</div>)}
      </main>
      {showCreate && (
        <NewAgentModal
          onClose={() => setShowCreate(false)}
          onCreated={(id) => { setShowCreate(false); select(id); }}
        />
      )}
      {showContext && <ContextOverlay onClose={() => setShowContext(false)} />}
      {showHelp && <ShortcutsHelp onClose={() => setShowHelp(false)} />}
      {authRequired && <TokenModal onSaved={onTokenSaved} />}
    </div>
  );
}
