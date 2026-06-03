// AttentionBar is the always-visible top strip: connection state, a count of
// agents that need the user (clicking jumps to Overview), the notifications
// toggle, and the New-agent action.
export default function AttentionBar({
  connected, attentionCount, notifyEnabled, onToggleNotify, onNew, onJumpAttention,
}: {
  connected: boolean;
  attentionCount: number;
  notifyEnabled: boolean;
  onToggleNotify: () => void;
  onNew: () => void;
  onJumpAttention: () => void;
}) {
  return (
    <header className="topbar">
      <h1>agentctl</h1>
      <span className={connected ? 'conn ok' : 'conn down'}>
        {connected ? 'live' : 'reconnecting…'}
      </span>
      {attentionCount > 0 && (
        <button className="attn-pill" onClick={onJumpAttention}>
          ⚠ {attentionCount} need{attentionCount === 1 ? 's' : ''} you
        </button>
      )}
      <button className="notify-toggle" onClick={onToggleNotify} title="Browser notifications when an agent needs input">
        {notifyEnabled ? '🔔 on' : '🔕 off'}
      </button>
      <button className="new-btn" onClick={onNew}>+ New agent</button>
    </header>
  );
}
