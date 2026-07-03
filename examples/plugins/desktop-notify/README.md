# desktop-notify — a compiled warden plugin

A worked example of warden's plugin system (#47) in its **recommended production
shape**: a single, statically-compiled Go binary that speaks the
JSON-over-stdio hook protocol.

Where [`../post-commit-notifier`](../post-commit-notifier) is the smallest
possible *shell* exercise of the protocol, this one goes a step further:

- **typed** request/response structs (mirroring `internal/plugin`'s wire types),
- **per-event policy** — it alerts loudly on a **failed check**, lightly on a
  commit or spawn, and stays silent on passing checks, and
- a **genuinely useful side effect** — an OS-native desktop notification
  (macOS `osascript`, Linux `notify-send`), with a log-file fallback so nothing
  is lost on a headless box.

Everything is **fail-open on both sides**: warden already logs-and-skips a
missing/slow/failing plugin (a hook can never gate an agent), and this binary
mirrors that — if the notifier is absent it appends to a log and still exits 0.

## Build

```sh
go build -o warden-notify ./examples/plugins/desktop-notify
```

(Stdlib only — no third-party deps — so `go build ./...` in this repo also
compiles it, which is how you'd want your own plugin gated in CI.)

## Register

In `~/.warden/config.yaml`:

```yaml
plugins:
  enabled: true                 # master switch — OFF by default
  registry:
    - name: desktop-notify
      path: /absolute/path/to/warden-notify
      events:
        - post-check            # ← the high-value one: alert on failure
        - post-commit
        - post-spawn
```

Restart the daemon, then confirm and drive it:

```sh
wd plugin list                  # shows it registered + which events it's on
wd check tests --agent dev-ab12 # a failing check → desktop popup
```

On a headless machine (no `notify-send`), notifications fall back to a log:

```sh
tail -f ~/.warden/plugin-desktop-notify.log   # override with WARDEN_NOTIFY_LOG
```

## The protocol, by example

warden writes one JSON request to stdin per subscribed event; the plugin writes
one advisory JSON response to stdout (or exits 0 silently to ack). You can test
it exactly as warden does, without a daemon:

```sh
printf '%s' '{"protocol_version":1,"event":"post-check",
  "session":{"id":"dev-ab12","type":"development","branch":"dev-ab12"},
  "payload":{"name":"tests","passed":"false"}}' | ./warden-notify
# → {"protocol_version":1,"ok":true,"message":"notified: ❌ check failed: tests"}
```

Payload fields available per event (from the dispatch sites in
`internal/daemon/strict_git.go` and `strict_lifecycle.go`):

| event         | payload keys                    |
| ------------- | ------------------------------- |
| `pre-spawn`   | *(none; session has only type/repo)* |
| `post-spawn`  | *(none)*                        |
| `pre-commit`  | `message`                       |
| `post-commit` | `sha`, `branch`, `committed`    |
| `pre-check`   | `name`                          |
| `post-check`  | `name`, `passed`                |

## Make it yours

`render()` in `main.go` is where the policy lives — that's the one function to
edit to change *when* and *what* you're alerted about. Swap `desktopNotify()`
for a Slack/Discord webhook `POST`, a `wall` broadcast, or an append to a
metrics file, and you have a different plugin with the same 40-line skeleton.
