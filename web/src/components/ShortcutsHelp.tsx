import { BINDINGS } from '../lib/shortcuts';

// ShortcutsHelp is the `?` overlay: a discoverable cheat-sheet of the global
// keyboard shortcuts. Backdrop click or the Close button dismisses it (Esc is
// handled by the global layer in Dashboard).
export default function ShortcutsHelp({ onClose }: { onClose: () => void }) {
  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal shortcuts-help" onClick={(e) => e.stopPropagation()}>
        <h2>Keyboard shortcuts</h2>
        <table className="shortcuts-table">
          <tbody>
            {BINDINGS.map((b) => (
              <tr key={b.keys}>
                <td><kbd>{b.keys}</kbd></td>
                <td>{b.label}</td>
              </tr>
            ))}
          </tbody>
        </table>
        <p className="muted">Shortcuts stay dormant while you're typing in a field.</p>
        <div className="actions">
          <button onClick={onClose}>Close</button>
        </div>
      </div>
    </div>
  );
}
