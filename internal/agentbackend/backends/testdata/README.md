# Backend conformance fixtures

This directory holds **real, versioned captures** of what each backend CLI
actually prints, used by the conformance harness in
`../conformance_test.go` to regression-lock warden's state detection against
silent TUI drift.

Each backend adapter infers an agent's coarse run state (idle / working /
needs-input) by scanning the captured tmux pane for marker substrings its CLI
prints. Those markers move when a CLI ships a new TUI — and when they do, warden
mis-reads the agent with nothing failing until a human notices in a live
session. The fixtures here pin the parsers to genuine captures so that drift
fails a test instead.

## Layout

```
testdata/<backend-id>/<scenario>.txt
```

- `<backend-id>` is the registered backend id (`codex`, `cursor`, `aider`, …).
- `<scenario>.txt` is a verbatim `tmux capture-pane -p` dump for one situation.

Canonical scenario names (add more as needed):

| file                        | situation                                   |
| --------------------------- | ------------------------------------------- |
| `state-idle.txt`            | agent at rest, composer ready               |
| `state-idle-after-turn.txt` | at rest immediately after a completed turn  |
| `state-working.txt`         | mid-turn, streaming a response              |
| `approval*.txt`             | an open approval / permission prompt        |
| `trust-prompt.txt`          | a first-run "trust this directory" prompt   |

> Non-`.txt` files in these dirs (`*.jsonl`, `*.json`, `*.md`) are transcript /
> export / model-list fixtures for other unit tests, not pane captures.

## Adding a fixture (new backend or new scenario)

1. Drive the real CLI in a tmux pane to the situation you want.
2. Capture the pane verbatim — include box-drawing and footer lines exactly as
   they appear, since that is what `DetectState` sees at runtime:
   ```sh
   tmux capture-pane -p -t <pane> > testdata/<backend>/state-working.txt
   ```
3. Add one row to `conformanceCases` in `../conformance_test.go` naming the
   backend, the fixture, the CLI version you captured against (`CapturedWith`),
   the expected neutral state, and whether it should parse as an approval.
4. `go test ./internal/agentbackend/backends/` — the new row must pass.

## Refreshing after a CLI update (versioning)

When a CLI update **moves a marker** and the conformance test fails:

1. Recapture the affected scenario into the **same** file, overwriting it.
2. Bump the `CapturedWith` version on that row in the manifest.

If you want to keep the **old** version's capture as an additional regression
fixture (proving the parser handles both the old and new TUI), copy it aside
with a version suffix and add a second manifest row for it:

```
testdata/codex/state-working.txt          # current
testdata/codex/state-working@0.142.3.txt  # retained older capture
```

The harness loads each fixture by its explicit path, so flat scenario names and
version-suffixed names both work with no code change.

## Optional live smoke test

`../conformance_live_test.go` is a cheap liveness check that invokes each
backend's real binary (`--help`) when it happens to be installed, to catch a
locally-upgraded or removed CLI before its fixtures are refreshed. It is
**opt-in** and never runs in the default `go test ./...`:

```sh
WARDEN_LIVE_BACKEND_TESTS=1 go test ./internal/agentbackend/backends/ -run TestBackendLiveSmoke -v
```

Backends whose binary is absent from PATH are skipped cleanly, so it works with
whatever subset of CLIs you have installed. It is a shallow probe (does the
binary run) — the deep marker check is the fixture conformance above.
