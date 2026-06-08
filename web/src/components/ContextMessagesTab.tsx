import { useEffect, useState } from 'react';
import type { ContextEntry, Message } from '../lib/types';
import { listContext, listMessages } from '../lib/api';
import { groupContext } from '../lib/inspector';

// ContextMessagesTab is a READ-ONLY inspector for the daemon's shared state: the
// namespaced context KV store agents write to, and recent inter-agent message
// traffic. It polls (the SSE channel carries only sessions) every 2s. Editing
// context / sending messages is done via the CLI + MCP, not here.
export default function ContextMessagesTab() {
  const [entries, setEntries] = useState<ContextEntry[]>([]);
  const [messages, setMessages] = useState<Message[]>([]);

  useEffect(() => {
    let on = true;
    const load = () => {
      listContext().then((e) => { if (on) setEntries(e); }).catch(() => { /* keep last */ });
      listMessages().then((m) => { if (on) setMessages(m); }).catch(() => { /* keep last */ });
    };
    load();
    const t = setInterval(load, 2000);
    return () => { on = false; clearInterval(t); };
  }, []);

  const groups = groupContext(entries);

  return (
    <div className="inspector">
      <section className="inspector-section">
        <h2>Shared context <span className="count">{entries.length}</span></h2>
        {entries.length === 0 && (
          <div className="empty">
            No shared context yet. Agents write keys with <code>agentctl ctx set</code>.
          </div>
        )}
        {groups.map((g) => (
          <div key={g.namespace} className="ctx-group">
            <h3 className="ctx-ns">{g.namespace}</h3>
            <table className="ctx-table">
              <tbody>
                {g.entries.map((e) => (
                  <tr key={e.key}>
                    <td className="ctx-key">{e.key}</td>
                    <td className="ctx-val"><pre>{e.value}</pre></td>
                    <td className="ctx-by" title={e.updated_at}>{e.updated_by}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ))}
      </section>

      <section className="inspector-section">
        <h2>Recent messages <span className="count">{messages.length}</span></h2>
        {messages.length === 0 && (
          <div className="empty">
            No messages yet. Agents talk with <code>agentctl msg send</code>.
          </div>
        )}
        {messages.length > 0 && (
          <ul className="msg-list">
            {messages.map((m) => (
              <li key={`${m.to}-${m.id}-${m.ts}`} className={`msg${m.read ? '' : ' unread'}`}>
                <span className="msg-route">
                  <span className="msg-from">{m.from}</span>
                  <span className="msg-arrow"> → </span>
                  <span className="msg-to">{m.to}</span>
                </span>
                <span className="msg-body">{m.body}</span>
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}
