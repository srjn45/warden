# Security Policy

## Reporting a vulnerability

Please report security issues privately — **do not** open a public issue for an
unfixed vulnerability.

- Preferred: GitHub private vulnerability reporting
  (repo → **Security** → **Report a vulnerability**).
- Or email: <srajanpathak45@gmail.com> with subject `warden security`.

Please include a description, the affected version/commit, and reproduction steps.
We aim to acknowledge within 5 business days.

## Threat model

warden is a self-hosted personal/team tool. By default the daemon binds
`127.0.0.1` and loopback requests are unauthenticated; non-loopback access
requires a 256-bit bearer token. A single token grants full control, and
authenticated callers include the agents warden spawns (so a prompt-injected agent
is partially in scope as an attacker). See the latest review under
[`docs/security/`](docs/security/) for known findings and their status.

## Supported versions

Security fixes target the latest release (the newest tag on the
[releases page](https://github.com/srjn45/warden/releases)); older release
lines are not patched.
