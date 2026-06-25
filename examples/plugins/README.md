# warden plugins (example)

warden's plugin system (#47) lets you extend warden with **custom agent task
types** and **lifecycle hooks** without rebuilding warden. A plugin is just an
**external executable** you register in config. warden invokes it over a simple
**JSON-over-stdio protocol** — a request on stdin, a response on stdout, bounded
by a hard timeout — the same shape as warden's built-in PreToolUse guard hooks.

The system is **off by default** (plugins execute external code) and every hook
is **advisory and fail-open**: a broken, slow, or missing plugin is logged and
skipped — it never blocks or crashes an agent.

See the full design in
[`docs/superpowers/specs/2026-06-25-warden-plugin-system-design.md`](../../docs/superpowers/specs/2026-06-25-warden-plugin-system-design.md).

## The protocol

For every subscribed lifecycle event warden writes one JSON request to the
plugin's stdin:

```json
{
  "protocol_version": 1,
  "event": "post-commit",
  "session": {
    "id": "dev-ab12", "type": "development", "repo": "/path",
    "worktree": ".worktrees/dev-ab12", "branch": "dev-ab12",
    "workdir": "/path/.worktrees/dev-ab12"
  },
  "payload": { "sha": "<sha>", "branch": "<branch>", "committed": "true" }
}
```

The plugin replies on stdout (or exits 0 with no output to silently ack):

```json
{ "protocol_version": 1, "ok": true, "message": "logged" }
```

The response is purely advisory — warden records `ok`/`message` but it never
changes warden's control flow.

### Hook events

`pre-spawn`, `post-spawn`, `pre-commit`, `post-commit`, `pre-check`,
`post-check`, `pre-teardown`. (`-spawn`, `-commit`, and `-check` are wired today.)

## Example: post-commit notifier

[`post-commit-notifier/notifier.sh`](post-commit-notifier/notifier.sh) appends a
line to a log file every time a hook fires — the smallest end-to-end exercise of
the protocol.

Register it by adding to `~/.warden/config.yaml`:

```yaml
plugins: true            # opt-in master switch (off by default)
plugin_registry:
  - name: notifier
    path: /absolute/path/to/examples/plugins/post-commit-notifier/notifier.sh
    events:
      - post-commit
      - post-spawn
```

Then restart the daemon and watch it fire:

```sh
wd plugin list                       # confirm it's registered + active
tail -f ~/.warden/plugin-notifier.log
```

## Example: a custom task type

A plugin can also declare custom agent task types, each with its own worktree
isolation policy (`worktree: true` isolates it in its own git worktree by
default, exactly like the built-in write-agent types):

```yaml
plugins: true
plugin_registry:
  - name: lint-bot
    path: /absolute/path/to/your/lint-bot
    events: [post-spawn]
    task_types:
      - name: lint-bot      # now a valid `wd start --type lint-bot ...`
        worktree: true
```

Custom type names must not collide with a built-in (`development`, `analysis`,
`spike`, `pr-review`, `code`, `docs`, `website`, `debug-ci`, `tests`, `other`)
or with another plugin's type — warden rejects the config at load time and runs
with plugins disabled if so (the error is logged; `wd plugin list` surfaces it
too).
