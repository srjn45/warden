---
title: "The database said zero. The agents were still running."
description: A debugging story about append-only storage. warden restarted its daemon and the cockpit came up empty — every agent still alive in tmux, but the store loaded zero records. The bug wasn't in the data. It was in the index, and it only fired after a segment rotation.
date: 2026-07-26
authors: srjn45
excerpt: I restarted the daemon and the cockpit came up with an empty fleet. tmux still had every agent, alive and mid-task. The append-only store had lost nothing — but it loaded zero records, because a segment rotation had corrupted the index over data that was sitting right there intact.
tags:
  - go
  - storage
  - internals
  - debugging
---

I restarted the warden daemon. The cockpit came up with an **empty fleet.**

Except it wasn't empty. `tmux ls` listed every agent session, alive, each one
mid-task. The processes were fine. The work was fine. The daemon had simply
loaded **zero records** from its session store on boot and concluded it was
supervising nobody.

If you read [the last post](/warden/blog/tmux-isolation-footguns/), this
symptom will feel familiar — *the supervisor insists the fleet is gone while
every agent is demonstrably alive.* Last time the lie came from tmux server
addressing. This time it came from one layer deeper: the **storage engine**.
And the twist is that the data was never lost. The *index over it* was.

## The store: append-only segments, keyed by id

warden keeps its state in an embedded store — [ScrivaDB](https://github.com/srjn45/scriva),
a small Go engine that every warden store embeds (sessions, pipelines,
snapshots, schedules, shared context). The session store is the one that bit me.
Its shape, straight from the code:

```go
// FileStore persists sessions in an embedded ScrivaDB rooted at
// <dir>/sessions-db/: live sessions in an "active" collection and archived
// ones in a "closed" collection, each record keyed by the session id.
// A write appends one record instead of rewriting a whole per-session JSON
// file (the write-amplification the previous FileStore carried).
```

The important word is **appends**. Each write is a new record appended to a
segment file; the engine keeps an **index** mapping each key to the latest
record for that key. Reads consult the index, not the raw segments. This is a
completely standard log-structured design, and it has a completely standard
sharp edge.

Append-only earns you one guarantee for free: you never tear a record
mid-write, because you never overwrite. warden's own comment leans on exactly
that —

```go
// …the last write surviving a power-loss is not a requirement
// (append-only segments rule out torn reads regardless).
```

But notice what that guarantee covers: **the data**. It says nothing about the
**index** — the derived structure that has to be maintained *alongside* the
append, and rebuilt or rotated as segments grow. That's the part that broke.

## Rotation: the rare path that only fires in production

Append-only logs can't grow forever, so the engine **rotates**: when the active
segment hits a size threshold, it's sealed and a new one starts, and the index
is updated to span the new layout. Rotation is the classic place storage bugs
hide, for a mean reason — **it only triggers after the store has grown past a
threshold.** In dev, with a handful of records, you never rotate. You ship. The
store fills up in production over days of real use, crosses the threshold, and
*then* the rare path runs for the first time, on your real data.

That's precisely what happened. The root cause was a **name collision during
segment rotation**: the rotation logic could reuse a segment name in a way that
left the index pointing at the wrong bytes. The append had succeeded — every
session record was physically there, on disk, intact. But after the rotation,
the index that was supposed to *find* those records was corrupt. On the next
open, the engine rebuilt its view from that index, found nothing coherent, and
handed warden an empty collection.

Zero records. Not an error — an *answer.* And that's the crux of what made it
nasty.

## "Zero records" is indistinguishable from "empty"

Here's the failure mode that turned a storage bug into a *warden* mystery: a
store that returns zero records looks exactly like a store that is legitimately
empty. There's no exception, no missing file, no torn read to catch. The daemon
asked "how many sessions?", got `0`, and did the only reasonable thing with that
answer — showed an empty cockpit.

The supervisor trusted a number it had no way to distinguish from a lie. Which
is the same shape as the tmux bug: the failing call didn't say *"you're asking
the wrong place"* — `has-session` just said *no*, and here the store just said
*zero*. Silent-wrong is so much worse than loud-wrong.

## Why nothing was actually lost

The reason this is a war story and not a data-loss postmortem is a design
decision that predates the bug: **warden treats `sessions-db/` as a derived
cache, not the source of truth.** The store's own boot path spells it out:

```go
// …if the sentinel is absent (never imported, or a prior attempt died
// partway) the derived sessions-db is wiped and rebuilt from the read-only
// legacy JSON, then the sentinel is written LAST — so a crash mid-import
// loses nothing.
```

The store keeps a sentinel file that means *"the database is authoritative."*
The database is populated (once) from plain JSON, and the sentinel is written
**last**. The invariant: nothing lives in the fast store that can't be
reconstructed from something more durable and dumber.

So the recovery from a corrupt index was, bluntly, to **throw the index away and
rebuild** — the exact same wipe-and-reimport path the store already runs when a
first-boot import is interrupted. The corruption surface and the crash-recovery
surface turned out to be the same surface, and the mitigation was already built.
That's not luck; it's the payoff of never letting derived state pretend to be
primary.

The real fix, of course, was upstream: the rotation name-collision was
root-caused and patched in ScrivaDB, and warden pinned the fixed version. But
the reason a rotation bug in the storage engine cost a restart and not a fleet
is the cache/source-of-truth split.

## The lessons

Two of them, and the first is the one I'd tattoo on a storage layer:

1. **Append-only protects the data, not the index.** "We never overwrite, so
   we can't corrupt" is only true of the records. Every derived structure laid
   over them — the index, the rotation bookkeeping, the compaction state — is a
   fresh corruption surface with none of the append guarantee. Audit those
   paths as if they were mutable, because they are.

2. **Rotation and compaction are where the bugs wait.** They're the code that
   only runs once the store is big *and* old — invisible in every test that
   starts from empty. If you write a log-structured store, force a rotation in
   your tests, then reopen and assert every key still resolves. The bug that
   ate my fleet would have surfaced the instant a test crossed the threshold and
   reopened.

And one structural rule that turned a scary bug into a boring one:

3. **Keep a dumb, durable source of truth under your fast store.** If your
   performant on-disk format can be wiped and rebuilt from something simpler,
   corruption is a rebuild, not a catastrophe. warden's session store can always
   fall back to plain JSON; that single decision is why "the database said zero"
   ended in a restart instead of a support thread.

A store returning zero is a valid answer to a question you didn't mean to ask.
Build the layer underneath so that when it does, you can just rebuild and move
on.

---

*warden is an open-source Go binary for running a fleet of coding agents from
one cockpit. The storage model — an embedded append-only store over a plain-JSON
source of truth — is described in the
[architecture docs](/warden/concepts/architecture/), or grab it from
[GitHub](https://github.com/srjn45/warden).*
