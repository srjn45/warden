import ContextMessagesTab from './ContextMessagesTab';

// ContextOverlay is the dismissible drawer that hosts the Context & Messages
// inspector. It used to be a top-level tab; it now opens from the 🗒 header
// button (AttentionBar) and renders as a right-side drawer over the app,
// mirroring the ShortcutsHelp overlay pattern. Backdrop click or the Close
// button dismisses it; Esc is handled by the global keyboard layer in Dashboard.
export default function ContextOverlay({ onClose }: { onClose: () => void }) {
  return (
    <div className="modal-backdrop" onClick={onClose}>
      <aside className="context-drawer" onClick={(e) => e.stopPropagation()}>
        <header className="context-drawer-head">
          <h2>Context &amp; Messages</h2>
          <button className="context-drawer-close" title="Close" onClick={onClose}>✕</button>
        </header>
        <div className="context-drawer-body">
          <ContextMessagesTab />
        </div>
      </aside>
    </div>
  );
}
