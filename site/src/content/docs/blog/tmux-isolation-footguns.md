---
title: "My whole agent fleet showed up dead. Every process was alive."
description: A debugging story about tmux server addressing. warden runs every coding agent in its own tmux session; one wrong environment variable made the supervisor report six live agents as tombstones. Three tmux isolation footguns — env inheritance, socket-path shape, and the $TMUX variable that quietly redirects every command.
date: 2026-07-27
authors: srjn45
excerpt: The cockpit showed six agents, all orphaned. `ps` showed six agents, all alive, happily working. Nothing had crashed. The bug wasn't in my code — it was in which tmux server my code was talking to. Three isolation footguns, and how each one bites.
tags:
  - go
  - tmux
  - internals
  - debugging
---

The cockpit showed six agents. Every one of them **orphaned** — the status
warden uses for "the session backing this agent is gone."

So I checked. `ps` showed six agent processes, all alive. Their tmux sessions
were there. The agents were, at that very moment, happily editing code in the
dark. Nothing had crashed, nothing had logged an error, and the supervisor was
certain the entire fleet was dead.

The bug wasn't in my code. It was in **which tmux server my code was talking
to** — and tmux never once told me I had the wrong one.

Here's the context you need, then the three footguns that took me embarrassingly
long to untangle.

## The model: one tmux session per agent

warden runs a fleet of coding agents. The design decision underneath all of it
is boring on paper: **every agent gets its own tmux session, and one daemon
supervises them all.** tmux buys persistence for free (detach, reattach, survive
a client crash), a stable handle to a live terminal, and a dead-simple way to
capture what an agent is doing right now (`capture-pane`). The daemon is the
single writer — it shells out to `tmux` to spawn, poke, and reap sessions, and
it decides an agent is alive by asking `tmux has-session -t <id>`.

That last line is the whole story. `has-session` answers a yes/no question —
*does this session exist?* — but it answers it **about a specific server**, and
which server it means is decided by ambient state you never explicitly named. If
the daemon asks the wrong server, every session "doesn't exist," and a perfectly
healthy fleet reports as orphaned. Which is exactly what I was staring at.

There are three separate ways to end up addressing the wrong server. They bite
in this order.

## Footgun 1: `$TMUX` hijacks every child tmux command

tmux sets an environment variable called `$TMUX` in **every process it
spawns** — it looks like `/tmp/tmux-1000/default,12345,0`. That variable is how
a tmux *client* knows which *server* it belongs to. Handy for interactive use.
A landmine for a supervisor.

Because when `$TMUX` is set, a bare `tmux` command binds to *that* server —
regardless of any other configuration you thought you set. So retrace my dead
fleet:

1. I'd started the daemon from inside a tmux session (I was already in one — who
   isn't?).
2. The daemon inherited `$TMUX` pointing at *my outer* tmux server.
3. The agent sessions lived on a different server than the one `$TMUX` named.
4. The daemon ran `tmux has-session -t agent-9d2e7b53` to check liveness. That
   command followed `$TMUX` to the **outer** server, which had never heard of
   `agent-9d2e7b53`.
5. `has-session` returned non-zero. The poller concluded the session was gone
   and marked the agent **orphaned** — six times over.

Nothing crashed. The agents were alive. tmux was doing exactly what it was told.
I'd just told it something I didn't mean to, via a variable I didn't set.

In Go this is trivially easy to do by accident, because `os/exec` **inherits the
parent environment when you don't set `Cmd.Env`**:

```go
func (ExecRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir            // we set the working directory…
	out, err := cmd.CombinedOutput()  // …but Cmd.Env is nil, so the child
	return string(out), err  // inherits the daemon's whole environment — $TMUX and all
}
```

`Cmd.Env == nil` means "use `os.Environ()`". So the daemon's environment *is*
every agent's tmux environment, transitively. The supervisor's launch context
silently became part of the architecture.

**The fix is to scrub `$TMUX` whenever a command must target a specific
server:**

```sh
env -u TMUX tmux has-session -t agent-9d2e7b53
```

The broader lesson is the one that generalizes past tmux: **for a long-lived
supervisor, the environment it launches in is not incidental — it's config.**
Pin it. That's why warden's daemon is meant to run from a known, clean
environment (a systemd unit, say) rather than "wherever I happened to type
`warden daemon`." A stray `$TMUX` in that environment is enough to make the
whole fleet look dead.

warden also leans on the *same* variable deliberately, on the client side —
the cockpit checks it to decide its layout:

```go
// InsideTmux reports whether we're running inside a tmux session.
func InsideTmux() bool { return os.Getenv("TMUX") != "" }
```

If you launch the TUI from inside tmux, it lays the cockpit out as native tmux
panes instead of nesting a second tmux inside the first. Same variable — a
signal when you read it on purpose, a footgun when you inherit it by accident.

## Footgun 2: the default socket is not where you think it is

Say you've learned footgun 1 and you want real isolation: a throwaway warden
instance for a demo, on its own tmux server, touching nothing real. So you set
`TMUX_TMPDIR` to point tmux at a private directory and move on.

Then you try to talk to that server explicitly with `-S` and guess the socket
path — and you guess wrong. The default tmux socket is **not**
`$TMUX_TMPDIR/default`. It's:

```
$TMUX_TMPDIR/tmux-<uid>/default
```

That `tmux-<uid>` subdirectory (e.g. `tmux-1000` for uid 1000) is easy to
forget, because you normally never type the socket path at all. The instant you
start passing `-S` by hand — in a script, a test harness, a capture rig — you
have to reconstruct the exact path tmux would have derived, `tmux-<uid>` and
all. Miss it and you've created a *second* empty server next to the one you
meant to reach, and now nothing lines up.

The cleaner move is usually to **not** pass `-S` at all: set `TMUX_TMPDIR`,
scrub `$TMUX`, and let tmux compute its own default socket underneath your
private tmpdir:

```sh
env -u TMUX TMUX_TMPDIR=/tmp/demo-tmux tmux new-session -d -s agent-demo
```

Let the tool derive the path. The moment you hard-code it, you've taken on a
piece of tmux's internal layout as your own responsibility.

## Footgun 3: precedence — `$TMUX` beats `TMUX_TMPDIR`

Footguns 1 and 2 combine into a nastier third one, and it's pure precedence.

Inside an existing tmux session, `$TMUX` is set. If you then set `TMUX_TMPDIR`
and run a bare `tmux`, you might expect the fresh `TMUX_TMPDIR` to win — you
just set it, after all. It doesn't. For a bare `tmux` client, **`$TMUX` wins**:
the command follows the inherited session to the *old* server and blithely
ignores the tmpdir you carefully pointed somewhere isolated.

So "I set `TMUX_TMPDIR`, therefore I'm isolated" is false while `$TMUX` is still
in the environment. Isolation requires *both*: scrub `$TMUX` **and** set
`TMUX_TMPDIR`. One without the other is a quiet no-op that looks like it worked
right up until two servers' worth of state bleed into each other.

```sh
# WRONG — $TMUX still set, so this targets the outer server despite TMUX_TMPDIR
TMUX_TMPDIR=/tmp/demo-tmux tmux new-session -d -s agent-demo

# RIGHT — scrub the inherited session first, then point at the private tmpdir
env -u TMUX TMUX_TMPDIR=/tmp/demo-tmux tmux new-session -d -s agent-demo
```

## The pattern behind all three

Every one of these is the same shape: **tmux resolves "which server" from
ambient state — an inherited variable, an implicit socket path — and the
resolution is invisible until it's wrong.** The commands never error in a way
that says "you're on the wrong server." `has-session` just says *no*.
`new-session` just quietly makes a second one. The state diverges silently and
you debug it as a *warden* problem (why is the fleet orphaned?) when it's really
a *tmux addressing* problem one layer down.

For anything that supervises tmux from code, three rules pay for themselves:

1. **Pin the environment.** A supervisor's launch environment is config. Scrub
   `$TMUX` for any command that must target a specific server, and don't let the
   daemon inherit an interactive shell's tmux context.
2. **Don't hand-author the socket path.** Set `TMUX_TMPDIR` and let tmux derive
   the `tmux-<uid>/default` socket itself. Reconstructing it by hand means you
   now own a piece of tmux's internal layout.
3. **Isolation is `env -u TMUX` *and* `TMUX_TMPDIR`, never one alone.**
   Precedence will punish the half-measure.

In warden these live at the boundary where the daemon shells out to `tmux`, and
they're the difference between "the cockpit shows six live agents" and "the
cockpit shows six tombstones while the agents keep happily working in the dark."

---

*warden is an open-source Go binary for running a fleet of coding agents from
one cockpit. If you want to see the tmux-per-agent model in action, the
[architecture docs](/warden/concepts/architecture/) start here, or grab it from
[GitHub](https://github.com/srjn45/warden).*
