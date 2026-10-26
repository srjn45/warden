# Data retention: archived agents, delivered messages, metrics

Status: implemented (v13.1.0)

## Problem

A long-lived install that had 6 projects, 5 live agents and ~150 plans carried
~85k warden-db records. The live working set was under 1% of that:

| Collection | Rows | Cause |
| --- | --- | --- |
| `metrics_samples` | 44,624 | 15 s sampling × 7-day window (already pruned daily) |
| `messages` | 19,233 | 17,768 **unread** messages addressed to archived agents; read messages survived 24 h and compaction only ran when that inbox received new mail |
| archived agents | 759 | no retention at all — archived records were kept forever |

Every row costs time on schema migrations, backups and full scans.

## Policy

```yaml
retention:
  archived_agents: 24h   # purge an archived agent this long after it was archived; "off" keeps forever
  read_messages: 0s      # keep a delivered message this long; 0s deletes it on delivery
  metrics_days: 7        # metrics history window
```

All three are configurable; the defaults above apply when a key is absent.

### Archived agents

- An archived agent stays restorable (`restore_agent`) for the whole window.
- Age is measured from `archived_at`. Records archived before that field
  existed fall back to `updated_at`.
- A daemon sweep runs at startup and then hourly. "24h" therefore means
  purged between 24 h and 25 h after archiving.
- Purge is a real delete of the archived row, in one transaction that refuses
  while any agent, pipeline or autopilot run still names it as
  `parent_agent_id`. Such a parent is retried on later sweeps, after its
  children age out. Live agents are never purged.
- A purged agent's inbox is deleted with it.
- One `retention_sweep` audit event summarises each sweep that removed
  anything, instead of one event per agent.

### Directed messages

- With `read_messages: 0s` (default), delivering a message deletes it:
  `GET /sessions/{id}/messages` (which already marked returned messages read)
  and `TakeFirstUnread` (wait-for-message) remove the message instead of
  flagging it read.
- A positive duration restores the previous keep-for-history behaviour, with
  the 500-message per-inbox cap unchanged.
- Unread messages are never dropped while their recipient exists. The sweep
  deletes inboxes whose recipient is neither a live nor an archived agent.

### Metrics

`metrics_days` replaces the hard-coded 7-day constant used by the daily prune.
The sampling interval is unchanged (15 s).

## Implementation

| Area | Change |
| --- | --- |
| `internal/config` | `RetentionConfig` + defaults + schema entry + normalisation and accessors |
| `internal/agentstore` | `Store.Purge(ctx, id)`: archived-only, reference-checked delete for schema 3 and the legacy store; snapshot publish drops the closed entry |
| `internal/mailbox` | configurable read retention; delete-on-delivery in `MarkRead`/`TakeFirstUnread` |
| `internal/daemon` | `runRetentionSweep` (startup + hourly); `SetRetention` wiring; metrics window from config |
| `internal/audit` | `ActionRetentionSweep` |

## Out of scope

- Sampling-interval changes and down-sampling for metrics.
- Retention for `audit_events`, `plan_events` and `savings_events`. These are
  small, bounded by activity and back history/savings views.
