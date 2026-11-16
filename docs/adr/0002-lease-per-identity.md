# ADR-0002 — Lease per identity, not per connection

- Status: accepted
- Date: 2026-11-17
- Deciders: maintainers

## Context

SSH clients open more than one TCP connection per logical session. Observed in the reference client
(an agent runtime using OpenSSH under the hood), one session produced at least two connections:

1. a `ControlMaster` connection used for `exec` (it carries a specific `SendEnv` set),
2. a plain socket used by `scp`/`tar` for file synchronization and by liveness probes.

`ControlMaster`/`ControlPersist` expiry, retries and parallel channel activity add more. Each *new TCP
connection* is a new authentication, so a gateway that keys its allocation on "the connection" hands out
a second instance in the middle of a session.

That is not merely wasteful, it silently corrupts any client that keeps bookkeeping between
connections:

- the client uploads a file tree once and remembers what it uploaded; the second connection lands on a
  different instance, so the tree is missing there and will never be re-uploaded;
- a recycled instance means the next command runs on an empty filesystem;
- a shared instance means one identity sees another's files.

Any of the three turns into "it worked, then it broke, and nothing in the logs explains it".

## Decision

The lease is keyed on **identity** (ADR-0001) and has the lifetime of *the session*, not of a socket:

- `lease(identity)` returns the same instance for as long as the lease is alive;
- every connection from that identity refreshes `last_seen`;
- an instance is reclaimed only after `ttl` (default 360s) of inactivity, plus a `grace` period;
- concurrent connections from one identity may share the instance (multiple channels on one target);
- the identity table is consulted *before* the pool, so a reconnect can never race with a recycle.

Parameters are chosen to exceed the client's own idle timers (the reference client idles out at 300s),
so a lease cannot expire while the client still believes it holds the instance.

## Consequences

- TTL must be configured per client population; too small reproduces the original bug, too large keeps
  instances idle. Default 360s = client idle + 20%.
- The gateway must survive its own restart without orphaning instances: on startup it adopts instances
  by their annotations (ADR-0006, M3) and re-creates the identity table lazily; a restarted gateway may
  legitimately give an identity a *new* instance, and clients that cache state must re-sync.
- A crash loses in-memory leases; the annotations are the only durable record. This is acceptable
  because the annotation is a hint, not a promise: the worst case is one lost file tree, immediately
  corrected by the client's next full sync.
- Because allocation is no longer 1:1 with sockets, "how many instances are in use" cannot be read off
  the connection count; it needs the lease table (exposed as `keypod_leases_active` in M3).

## Alternatives rejected

| alternative | why not |
|---|---|
| one instance per TCP connection | the bug described above; the whole project exists to avoid it |
| sticky by source IP | breaks behind NAT/proxies and does not survive client restarts |
| long-lived static assignment per key | defeats pooling; instances are not reclaimed, cost grows without bound |
| ask the client to keep a single connection | not under our control; `scp` and `ControlMaster` are separate by design |
