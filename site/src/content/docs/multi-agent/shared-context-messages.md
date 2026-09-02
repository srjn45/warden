---
title: Shared context & messages
description: A namespaced key/value blackboard and per-agent directed messages — the substrate pipelines are built on.
---

The substrate pipelines are built on, usable directly so agents can collaborate.

## Shared context (`warden context`)

A namespaced key/value store the daemon owns, so agents can share results.

```sh
warden context set global.findings "auth.py needs refactor"   # inline value
warden context set report.body --file ./report.md             # value from a file
some-command | warden context set logs.tail --stdin           # value from stdin
warden context get global.findings                            # prints the value
warden context list pipeline.                                 # keys under a prefix
warden context delete global.findings
```

Writes are attributed to `$WARDEN_SESSION_ID` when set (so a spawned agent's writes are tagged with its id), otherwise to `human`. Override with `--as`. Keys are free-form dot-namespaced strings (`global.*`, `pipeline.<id>.*`, `agent.<sid>.*`). Also available as MCP tools `ctx_set` / `ctx_get` / `ctx_list`.

## Directed messages (`warden message`)

Agent-to-agent messages with a durable per-recipient inbox.

```sh
warden message send <agent-id> "can you check the auth module?"   # deliver + wake if idle
warden message inbox                                              # read my messages (marks read)
warden message inbox --unread                                     # only unread
warden message wait --from <agent-id> --timeout 120               # block until a reply (one call)
```

Sending **wakes the recipient only if it's idle or waiting** — a working agent is never interrupted; its message waits in the inbox. `msg wait` blocks in the daemon (a long-poll), so an agent awaits a reply in a single call with no busy-loop. Identity defaults to `$WARDEN_SESSION_ID`, which warden sets on every agent's tmux session automatically — so inside an agent, `msg` and `ctx` commands just work without flags. Pass `--as <agent-id>` only to act as a different agent (e.g. a human operator or a lead agent answering on another's behalf). Also available as MCP tools `send_message` / `read_inbox` / `wait_for_message` (the blocking long-poll), so an orchestrator agent can do the whole exchange over MCP.

Request/reply pattern: A runs `msg send B "..."` then `msg wait --from B`; B reads its inbox, does the work, and replies with `msg send A "..."`, unblocking A.
