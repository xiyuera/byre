# ADR-0008 — A lease is invalid the moment its instance is gone

- Status: accepted
- Date: 2026-11-17
- Deciders: maintainers

## Context

On 2026-11-17, between 14:24 and 14:26, every profile that points at the gateway saw the same sequence:
`Session open refused` → `connection refused` on the instance port → then DNS failures. The migration
window was blamed first. It was not the cause; it only made the defect visible.

The defect is in the lease: **a lease outlived the object it named.**

1. `(*Manager).Acquire`, reuse branch, did `refs++`, `LastSeen = now` and returned the entry. No liveness
   check, no re-list, no probe.
2. `(*Server).handleConn` dialled `entry.Instance`; on failure it logged `cannot reach instance` and
   closed the client. It did not drop the lease, did not mark it, did not re-acquire.
3. Invalidation happened only in `Reap`, i.e. at `ttl + grace` — six minutes plus one by default.

So for seven minutes after a pod was deleted, every reconnect from that identity was forwarded to an
address that no longer existed. The client's only evidence was a closed connection: it could not see that
the gateway was dialling a grave.

Nothing on the object can answer the question at adoption time either. A deleted object is not returned by
`List`, so whatever it was annotated with is simply absent. **Absence has to be established by a positive
query, not by comparing a remembered value with a listed one.**

## Decision

### 1. Every reuse of a lease is checked against the backend

`backend.Liveness` is a new optional interface:

```go
Alive(ctx context.Context, in Instance) error
```

- `nil` — the instance exists;
- an error wrapping `backend.ErrInstanceGone` — it is definitively gone (deleted, or terminating);
- any other error — the answer is unknown.

The Kubernetes driver implements it with **one `Get` on the object's name**, bounded by
`defaultLivenessTimeout` (2 s). The check runs on the reuse path in `Acquire`, not on a timer: the window
being closed is exactly one connection, and a timer only reproduces the defect with a shorter delay.

The three outcomes are treated differently on purpose:

| outcome | action | why |
|---|---|---|
| alive | reuse the lease | the ordinary case |
| gone | drop the entry, count `byre_lease_dead_instance_total`, provision a replacement | the client needs an instance, not an error |
| unknown | keep the lease, count `byre_lease_liveness_errors_total`, log | an apiserver blip must not cost a client its files |

### 2. A failed dial is classified, not just logged

`handleConn` no longer closes the client on the first dial error. `Manager.DiscardIfGone` asks the driver
whether the object is still there:

- **gone** → the entry is dropped and one replacement is acquired and dialled *inside the same connection
  attempt*. If the replacement is reachable, the client's session opens on it and the client never sees an
  error at all. One retry, not a loop.
- **alive** → the lease is kept (the client's own instance is intact; replacing it would discard state it is
  entitled to), the attempt is counted as `byre_lease_unreachable_total`, and the client is disconnected as
  before.

The distinction is made by the backend, not by matching error strings: an `ECONNREFUSED` means the same
thing at the socket layer in both cases and different things to the user.

### 3. Drivers that cannot answer are unchanged

`endpoints` (a fixed host list) and `command` (an external provisioner) have no object to ask. They do not
implement `Liveness`, so their leases behave exactly as before. This is deliberate: inventing a liveness
notion for them (a TCP connect, a command invocation) would be a different decision with its own failure
modes, and the incident was not about them.

## Why this is not an extension of the UID annotation

A later keypod-side variant of this code added a `keypod.io/uid` annotation, and it is worth being explicit
about why that is a different mechanism and does not replace this one.

The UID annotation answers: *"the object behind this name was replaced by a different object"*. It is read
from the annotations of objects that `List` returns, and compared during adoption. It is silent in two cases:

- the object is **gone**: nothing is returned, so there is no annotation to compare, and no UID to mismatch;
- the object is gone **and the name was not reused**: the case in the incident.

An annotation records a fact at write time. This ADR asks the API server for a fact at read time. The two
answer different questions — "is this the same object as before?" versus "is there an object at all?" — and
only the second one can be answered when the answer is "no".

This ADR therefore adds no UID field to `backend.Instance` and no annotation of its own. If name-replacement
detection is wanted later, it belongs on top of this check (a UID mismatch would be one more `gone` reason),
not instead of it.

## Why not a full `List` on the hot path

The question is about exactly one object, and the reconnect path is the one that has to stay cheap: the
warm pool exists to turn a thirty-second cold start into a three-hundred-millisecond connect. A namespace-wide
`List` per reconnect would:

- cost time proportional to the number of instances in the namespace, not to the one lease being checked;
- return every object body, when the answer wanted is a single boolean;
- turn a per-client event into API load that scales with the number of clients reconnecting at once — which
  is what happens during an incident, the moment the check matters most.

A `Get` on a name is O(1) for the API server, is served from its watch cache in the common case, and is
answered for one object. If API QPS from reconnects ever becomes the bottleneck, the next step is a shared
informer with a local index (state, RBAC for list+watch, startup warm-up), not a list per connection.

## Alternatives rejected

| alternative | why not |
|---|---|
| shorter `ttl` / `grace` | the failure window must be zero, not smaller; and the TTL also bounds *legitimate* reconnects, so shortening it destroys working sessions to fix a dead one |
| informer/watch cache now | correct, but it introduces state, a second source of truth and a warm-up phase; the measured cost of one `Get` per reconnect does not justify it yet (see above for when it would) |
| `ownerReferences` | rejected in ADR-0007 §3: a rollout cascades into deleting leased instances, garbage collection deletes without a reason, and ownership points the wrong way |
| relying on the readiness probe | ADR-0005's probe answers "is this dialable now", at acquire time; it cannot answer "is it still there in ten minutes" |
| probing the address instead of the object | a TCP probe against a dead pod's IP is what already failed; it also cannot distinguish "gone" from "not answering" |

## What this ADR does not do

- **No leader election and no CAS retry.** ADR-0007's gate for `replicas: 2` is unchanged: one writer per
  lease table remains an assumption, and the counters added here are per process.
- **No client-visible error message.** `golang.org/x/crypto/ssh` exposes no server-side API to send
  `SSH_MSG_DISCONNECT`, so a gateway that has to close a connection can only close it. The improvement is
  that it closes it *after* trying the replacement: the client either gets a session on a new instance, or
  one disconnection for a genuinely unusable instance — instead of one per reconnect for seven minutes.

## Consequences

- One extra `Get` per reconnect, per identity. Bounded at 2 s, and the timeout produces "unknown", never
  "gone".
- A terminating object is treated as gone, so a client is re-homed before the process disappears rather than
  after.
- An unreachable apiserver keeps leases (fail-open toward preserving state), and shows up as
  `byre_lease_liveness_errors_total`.
- Three counters make the event legible: `byre_lease_dead_instance_total`,
  `byre_lease_liveness_errors_total`, `byre_lease_unreachable_total`.
- The regression tests are built on a fake clientset that deletes a real pod object, and one of them asserts
  that the client's session is served by the replacement instance. Making the check a no-op turns them red.
