# ADR-0015 — A claim is a stable handle; its address is not

- Status: accepted
- Date: 2026-11-27
- Deciders: maintainers
- Corrects: the reuse path's assumption that an instance that exists is dialled at the address it was
  created with, and adoption's assumption that somebody else records its rows

## Context

Measured on a live gateway, 2026-11-27 12:10–12:38 CST. Two identities lost every session for 28
minutes with `mux_client_request_session: read from master failed: Broken pipe`. The first reading —
"a lease points at a deleted instance" — was wrong, and the way it was wrong is the point of this ADR.

`byre-76e13749` and `byre-fdfad37d` are **SandboxClaims**. They had existed for 46 hours and were READY
the whole time. What changed at 12:10 is that someone deleted five Sandbox objects; the claim controller
rebuilt each one under a *new* name:

| claim | sandbox before | sandbox after |
|---|---|---|
| byre-76e13749 | sandbox-2b47c | sandbox-vk77n |
| byre-fdfad37d | sandbox-5lrgc | sandbox-4bgbq |

Three facts did the rest:

1. `Kubernetes.Alive` on the claim path GETs the **claim** and returns nil when it is there. It never
   looks at `status.sandbox`, so "the object behind the lease was replaced" is indistinguishable from
   "nothing happened".
2. `Instance.Addr` was assigned exactly once, in `instance()`, at creation. Nothing refreshed it.
3. `Manager.Acquire`'s reuse branch returned the snapshot as soon as `Alive` said the instance existed.

So every reconnect was dialled at `sandbox-2b47c…`, a name that no longer resolved. Reuse is what the
lease is *for*, so the failure was total rather than occasional: `byre_lease_dead_instance_total` stayed
at zero, because nothing asked a question that could answer "gone".

Two things that were believed to be remedies were not. With `ttl: 24h` there was no self-healing: the
12:22 restart fixed it only because adoption re-read the claim status from scratch. And deleting the
`ByreLease` objects at 12:38 changed nothing while the process ran — `Store.Watch` has no caller, and
the lease table is the process's memory.

A second defect surfaced in the same investigation: adoption filled `byIdentity` and stopped there.
When the store could not be read at startup, `Recover` fell back to `Adopt`, whose adopted leases were
never written; the count in memory was 2 while `kubectl get byreleases` returned nothing, and
`byre_leases_unpersisted` reported **0** because the gauge only knew about writes that failed, never
about rows that were never attempted.

## Decision

**Drivers may report that their instance's address moved. The lease manager asks on every reuse.**

A new optional interface, next to `Liveness`:

```go
type Refresher interface {
    Refresh(ctx context.Context, in Instance) (Instance, error)
}
```

`Kubernetes` implements it for `claims` with one GET, re-deriving the address from the status it reads
now through the same `endpointCandidates` the readiness path uses (configured status field, then
`status.sandbox.podIPs`). `workloads` returns the instance unchanged: a pod's address is part of the pod,
and a deleted or replaced pod is what `Alive` already reports. A claim that advertises no address is
*not* reported as gone — the controller is between a deleted sandbox and its replacement, and the cached
address may still answer.

The reuse path becomes: `Alive` first (unchanged — a claim that really is gone still drops the lease and
provisions), then `Refresh`. Three outcomes:

| outcome | lease | counter |
|---|---|---|
| address, changed | row's `Addr` updated in place, same instance ID, same lease | neither moves |
| `ErrInstanceGone` | dropped, replacement provisioned | `byre_lease_dead_instance_total` |
| anything else ("unknown") | kept with the cached address | `byre_lease_liveness_errors_total` |

The update is guarded by instance ID, so a refresh that raced a replacement cannot stamp an old address
onto a new row. A change is logged once:

```json
{"level":"INFO","msg":"lease address refreshed","identity":"…","backend":"k8s-claims","instance":"byre-76e13749","old":"sandbox-2b47c.…:22","new":"sandbox-vk77n.…:22"}
```

**Adoption writes its own rows.** `adopt` records every lease it takes over from an annotation alone
(one `Put` each, after the table is committed and outside the lock). A `Put` that loses a race to
another replica is resolved by re-reading the row: same instance ⇒ nothing to do; a different instance ⇒
the store wins (ADR-0013), the row is not ours, and the lease is marked unpersisted so the gauge says so.
`Recover` no longer has a second write pass of its own; the one path covers both, including the fallback
where the store could not be listed at startup.

## Alternatives considered

- **Drop the lease when the sandbox changed.** Rejected: the claim is the handle the client's files live
  behind, and a replacement sandbox under the same claim is the *expected* lifecycle of a claim. Dropping
  would count a dead instance for something that never died, would reset `refs` for every connection on
  that identity, and would leave a drain state with nothing to drain.
- **Refresh on a timer.** Rejected: it reproduces the defect with a shorter delay. The window that
  matters is one connection, and the reuse path is exactly that window.
- **Store the address in the `ByreLease` CR and let a watcher push changes.** Rejected for now: the
  address is derivable from the object the lease names, and a second copy would need its own invalidation
  story. It also inherits the gap below — nothing consumes `Store.Watch` yet.
- **Have the manager drop the lease when the claim's UID changes** (a claim deleted and recreated under
  the same name). Not done here: it is a different question — whether a *new* claim object may inherit
  the old lease's client — and it changes what happens to a client's files. Left as a known gap rather
  than smuggled in with the address fix.
- **Mark adopted leases unpersisted instead of writing them.** Rejected: it makes the gauge honest but
  leaves one replica's leases invisible to the other, which is the defect the store exists to prevent.

## Consequences

- One extra single-object GET per reuse, on `claims` backends only, bounded by the same
  `livenessTimeout` as `Alive`. `workloads` and drivers without `Refresher` are unaffected.
- `byre_leases_unpersisted` now means "the store does not know this lease, or knows a different one for
  this identity" — including leases adopted at startup whose write failed. A flat non-zero after a
  restart is a store problem, not a leftover.
- `byre_lease_dead_instance_total` no longer moves for an instance that was replaced behind a stable
  name. An operator who saw it stay at zero during this incident should read that as "the question was
  never asked", not as "nothing died" — that is what the new log line is for.
- Known gaps, deliberately not addressed here: a `ByreLease` object deleted while the process runs is
  still not recreated (`Store.Watch` has no caller), and a claim deleted and recreated under the same
  name is still not treated as a different instance by `Alive`.
- Verified by `TestAcquireRefreshesAClaimAddressWhenTheSandboxIsReplaced` (address moves, one lease,
  `dead_instance_total` unchanged), `TestAcquireKeepsTheCachedAddressWhenTheClaimAdvertisesNone`
  (unknown keeps the old address and counts once), `TestAcquireReplacesADeletedClaim` (the complement),
  and `TestAdoptPersistsEveryLeaseItTakesOver` / `TestAdoptPersistFailureIsVisibleInTheGauge` (rows ==
  leases, or the gauge == the leases the store does not know).
