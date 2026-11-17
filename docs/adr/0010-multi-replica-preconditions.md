# ADR-0010 — A second replica needs leader election or CAS; instances are not owned by the Deployment

- Status: accepted
- Date: 2026-11-17
- Deciders: maintainers
- Supersedes: nothing. Qualifies ADR-0002 (one live lease per identity) for the multi-process case.

## Context

The truth about who holds which instance lives in two places: an in-process table (`internal/lease`) and
the `byre.io/identity` annotation on the instance object. ADR-0002 states the invariant both exist to
protect — one identity has at most one live lease — and today that invariant is enforced by a mutex.

Two writes carry it:

- `annotate()` (`internal/backend/kubernetes.go`) merges `byre.io/identity`, `byre.io/instance` and
  `byre.io/uid` onto the object with `MergePatchType` and **no precondition**;
- start-up adoption lists every annotated object and rebuilds the table from what it reads.

With one replica this is sound: the manager's lock serialises acquire-and-bind, so two identities cannot
bind the same object concurrently, and nothing else writes those keys.

With two replicas each of those statements stops being true. Both processes adopt the same object for the
same identity, both consider themselves the holder, and both hand the same instance to a client. The two
clients then write into one filesystem while each believes it is alone — the failure ADR-0002 exists to
prevent, made invisible because every process's own log looks correct.

The same review asked a second question: should reclamation use `ownerReferences` and let the garbage
collector delete instances when the gateway goes away?

## Decision

### 1. No `resourceVersion` precondition while there is one replica

It would cost a GET per bind (`annotate` currently needs none) and it cannot make two writers correct — it
only turns silent double-claim into a conflict that one of them has to handle. Adding it now buys nothing
and would suggest that two replicas are supported.

### 2. Two replicas are gated on one of two mechanisms

Before `replicas: 2` is set anywhere, the code must have one of:

| mechanism | what it must provide |
|---|---|
| **leader election** (preferred) | only the leader creates, binds and releases; followers either proxy to it or stand by. No object is written by two processes, so the annotation stays a single-writer record. |
| **CAS retry** | `annotate()` writes with a `resourceVersion` precondition (test-and-set on the object), the acquire path retries on conflict, and the lease table becomes a cache of a decision the API server arbitrated rather than the decision itself. |

Either is acceptable; both must be demonstrated with two processes against one fake API server that
re-orders writes, because the bug being prevented is not reproducible with one.

The UID annotation added in v0.1.3 is a safety net, not a mechanism: it detects "the object I bound is
gone" (name reused) but says nothing about "someone else bound it".

### 3. `ownerReferences` rejected

Instances outlive the gateway on purpose. Adoption after a restart is a feature (ADR-0002 and
`docs/architecture.md`): a client that reconnects keeps its instance and does not re-sync. An
`ownerReference` from instance to Deployment inverts that:

- a rollout (`kubectl apply`, image change), a scale-down or `kubectl delete deploy/byre` cascades to
  **every leased instance**, deleting client state on an operation that is supposed to be a gateway
  upgrade;
- the GC deletes asynchronously and without a reason field, so the deletion is indistinguishable from a
  crash and cannot be asserted on;
- ownership is the wrong direction of truth: the lease owns the instance, the gateway merely holds leases.

Reclamation therefore stays explicit: the reaper releases expired leases, `Release` deletes the object,
and adoption at start-up destroys unbound leftovers. All three are observable (log line plus a counter).

## Consequences

- `deploy/deployment.yaml` keeps `replicas: 1` and says why; `docs/roadmap.md` v0.3 keeps the two designs.
- The failure mode of getting this wrong is silent: two clients on one instance, each seeing a consistent
  world. That is the reason this is written down rather than learned in production.
- `byre.io/uid` (v0.1.3) makes the single-replica case stricter — a recreated object no longer inherits
  the old identity — and remains correct under either future mechanism.
- When multi-replica arrives, this ADR is superseded rather than amended: the decision it records is
  "not yet, and here is the gate".

## Alternatives rejected

| alternative | why not |
|---|---|
| shard by identity hash across replicas, no coordination | a client's fingerprint is only known after authentication; the SSH front end would have to route by key to the right replica, which needs a shared map — the thing being avoided |
| "sticky" Service with client IP affinity | NAT and reconnect reuse make affinity a guess; a wrong guess is double-claim |
| put the lease in an annotation with `ownerReferences` and trust GC to clean up | cascade deletes leased instances on every rollout; see above |
| one replica with a hot standby that takes over only on failure | needs a shared, monotonic lease record anyway — leader election with a different name |
| accept double-claim and rely on the client to notice | clients do not notice: both sessions succeed, files interleave |
