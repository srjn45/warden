# Contributing to warden

Thanks for your interest in warden! This guide covers the legal/process basics;
see [`CLAUDE.md`](CLAUDE.md) for engineering conventions and the
[docs](https://srjn45.github.io/warden/) for how the project is built.

## Licensing of contributions

warden is licensed under the **Apache License, Version 2.0**. By submitting a
contribution (a pull request, patch, or any other work) you agree that it is
provided under the same Apache-2.0 license, per section 5 of that license
("Submission of Contributions"). You retain copyright to your contribution.

Please contribute only code you have the right to submit — your own work, or work
you are authorized to contribute under a compatible license. Don't paste in code
under a copyleft (GPL/LGPL/AGPL) or other incompatible license: warden ships as a
permissively-licensed binary and all current dependencies are permissive (see
[`THIRD-PARTY-NOTICES.md`](THIRD-PARTY-NOTICES.md)).

## Developer Certificate of Origin (DCO)

To make provenance explicit, we use the
[Developer Certificate of Origin](https://developercertificate.org/). It is a
simple statement that you wrote, or otherwise have the right to submit, the code
you are contributing. **Sign off each commit** by adding a `Signed-off-by` line:

```sh
git commit -s -m "your message"
# adds: Signed-off-by: Your Name <you@example.com>
```

Use a real name and an email you can be reached at. Sign-off certifies the DCO
(reproduced at <https://developercertificate.org/>).

## Adding or updating dependencies

If you change `go.mod`/`go.sum`, regenerate the third-party notices so the
attribution stays accurate (CI enforces this):

```sh
go install github.com/google/go-licenses@latest
./scripts/gen-notices.sh
git add THIRD-PARTY-NOTICES.md
```

Avoid pulling in copyleft-licensed modules; if a dependency's license is unclear,
flag it in the PR.

## Before you open a PR

- Run the project checks (`make` targets / `warden check`); the repo's pre-commit
  and pre-push hooks run `gofmt`, `go vet`, and the fast test suite, and there is
  no `--no-verify` bypass.
- Update the docs the change touches — see the Definition-of-Done checklist in
  [`CLAUDE.md`](CLAUDE.md) (README, `docs/`, the website under `site/`, and the
  skill).

## Trademarks

"warden" and the warden marks are the project's own. References to third-party
products (Claude, Codex, Cursor, etc.) are nominative — for identification only —
and imply no affiliation. See the **Trademarks & affiliation** note in the
[README](README.md).

## Reporting security issues

Please **do not** open a public issue for a vulnerability. Use the private channel
in [`SECURITY.md`](SECURITY.md).
