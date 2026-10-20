---
title: Relay contract and security boundary
description: What the shipped node-side relay contract guarantees, and which Hub features remain planned.
---

Warden includes the node-side relay wire contract and the daemon's accept-side
authorization seam. They are building blocks for a future Warden Hub—not a
complete public remote-access service. For usable remote access today, use
[bearer-token remote access](/warden/guides/remote-access/) over a private network
or authenticated tunnel.

## The shipped contract

The public `relay/wire` package defines versioned frames and the three relay
legs:

1. Enrollment or device authorization provisions a node identity.
2. A node-to-Hub transport carries multiplexed streams.
3. A client stream carries the daemon's existing HTTP API.

The daemon's `AcceptRelayStream` seam validates a Hub-opened stream and maps its
effective scope into the normal daemon authorization rules. No separate relay
HTTP API exists: a successful stream is intended to serve the same versioned
node API described in [the API reference](/warden/guides/node-api-client-contract/).

For native end-to-end streams, the effective permission is the narrower of the
client certificate's scope and the Hub header's scope. A Hub cannot use its
header to expand a native client's authority.

## Browser-terminated streams are off by default

A browser cannot supply the native client certificate in this design. Its
Hub-terminated stream is therefore a distinct mode and is rejected unless this
daemon setting is explicitly enabled:

```yaml
# ~/.warden/config.yaml
relay:
  allow_web_terminated: true
```

When disabled, the daemon rejects that stream type with relay close code `4004`.
When enabled, the daemon trusts the Hub's asserted grantee and scope for that
stream. A read-only relay scope is still denied writes and interactive terminal
attach, just like the local read-only bearer token.

## What is not available yet

The repository does not ship a generally available Hub service, a daemon
dial-out connector, remote-node discovery, or a client command to connect to a
remote node. Treat the wire package, `warden login`, and the accept-side policy
as compatibility work for those planned pieces—not as instructions for exposing
a daemon on the public internet.

