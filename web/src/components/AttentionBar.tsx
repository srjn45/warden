import { THEME_ICON, THEME_LABEL, type Theme, type Resolved } from '../lib/theme';

// AttentionBar is the always-visible top strip: connection state, a count of
// agents that need the user (clicking jumps to the Others tab), the Context &
// Messages overlay toggle, the notifications toggle, the theme
// (light/dark/system) toggle, and the New-agent action.
export default function AttentionBar({
  connected, attentionCount, notifyEnabled, onToggleNotify, onNew, onJumpAttention,
  tokenSet, onClearToken, theme, resolvedTheme, onCycleTheme, onShowHelp, onToggleContext,
  onOpenTui, onToggleAutopilot,
}: {
  connected: boolean;
  attentionCount: number;
  notifyEnabled: boolean;
  onToggleNotify: () => void;
  onNew: () => void;
  onJumpAttention: () => void;
  tokenSet: boolean;
  onClearToken: () => void;
  theme: Theme;
  resolvedTheme: Resolved;
  onCycleTheme: () => void;
  onShowHelp: () => void;
  onToggleContext: () => void;
  onOpenTui: () => void;
  onToggleAutopilot: () => void;
}) {
  // Pick the wordmark for the theme that actually renders, so an explicit
  // override (not just the OS) gets the matching asset.
  const wordmark = resolvedTheme === 'dark'
    ? '/brand/warden-wordmark-dark.svg'
    : '/brand/warden-wordmark-light.svg';
  return (
    <header className="topbar">
      <h1 className="brand">
        <img src={wordmark} alt="warden" />
      </h1>
      <span className={connected ? 'conn ok' : 'conn down'}>
        {connected ? 'live' : 'reconnecting…'}
      </span>
      {attentionCount > 0 && (
        <button className="attn-pill" onClick={onJumpAttention}>
          ⚠ {attentionCount} need{attentionCount === 1 ? 's' : ''} you
        </button>
      )}
      {/* Action controls grouped flush-right with even spacing. */}
      <div className="topbar-actions">
        <button
          className="autopilot-btn"
          onClick={onToggleAutopilot}
          title="Autopilot — toggle and status"
          aria-label="Autopilot toggle and status"
        >
          ⚙ autopilot
        </button>
        <button
          className="tui-launch"
          onClick={onOpenTui}
          title="Open the full-screen TUI cockpit (Ctrl+Q to exit)"
          aria-label="Open the full-screen TUI cockpit"
        >
          ▢ TUI
        </button>
        <button
          className="theme-toggle"
          onClick={onCycleTheme}
          title={`Theme: ${THEME_LABEL[theme]} (click to change)`}
          aria-label={`Theme: ${THEME_LABEL[theme]}. Click to change.`}
        >
          {THEME_ICON[theme]} {THEME_LABEL[theme]}
        </button>
        <button
          className="context-toggle"
          onClick={onToggleContext}
          title="Context & Messages"
          aria-label="Open Context & Messages"
        >
          🗒
        </button>
        <button
          className="help-toggle"
          onClick={onShowHelp}
          title="Keyboard shortcuts (press ?)"
          aria-label="Show keyboard shortcuts"
        >
          ?
        </button>
        <button className="notify-toggle" onClick={onToggleNotify} title="Browser notifications when an agent needs input">
          {notifyEnabled ? '🔔 on' : '🔕 off'}
        </button>
        {tokenSet && (
          <button className="token-clear" onClick={onClearToken} title="Forget the stored access token on this device">
            🔑 sign out
          </button>
        )}
        <button className="new-btn" onClick={onNew}>+ New agent</button>
      </div>
    </header>
  );
}
