# ADR-0013 — Lease state belongs in a CRD; memory holds only what is derived

- Status: accepted (implementation in progress; see Implementation)
- Date: 2026-11-20
- Deciders: maintainers
- Qualifies: ADR-0002 (one live lease per identity), ADR-0010 (multi-replica preconditions)

## Implementation

Landing in parts, because a single change to the acquisition path is a regression nobody sees until an
operator's ssh stops working.

| part | what | state |
|---|---|---|
| 1 | `ByreLease` CRD, `internal/lease/crd` (get/put/update/setPhase/delete/list/watch), RBAC | landed |
| 2 | `LeaseStore` interface; `Acquire` writes the binding, `Release` deletes it; failures counted | landed |
| 3 | startup reads the bindings instead of relying on annotations | landed |
| 4 | watch: an instance object that goes away moves its lease to `Draining` | landed |
| 5 | connection-level demotion (see below) | under discussion |

Parts 1–2 change no behaviour on their own: with no store configured the manager keeps its table in
memory, which stays the correct mode for `endpoints` and `command` — those provision nothing, so there
is nothing for a second replica to agree about.

**Part 3 is the one that stops the recurring incident.** It was placed after the watch when this ADR
was written; it is now before it, because the failure that keeps happening is not "the object was
deleted" but "the process restarted and forgot everything".

## A second failure mode this ADR did not anticipate

The Context above describes an instance that is **deleted**. In practice the more common shape is an
instance that is **still there and no longer usable**:

```
container OOM-killed and restarted
→ the object is untouched: the pod is still Running, the claim still exists
→ no Deleted event, and Alive()'s GET answers "it is still there"
→ sshd inside the container refuses new sessions until it is ready again
→ every reconnect is handed an instance that answers "session open refused by peer"
```

Neither the watch in part 4 nor `Alive()` catches this: both ask about the **object**, and the object is
fine. What is broken is reachability. The only signal that carries this information is the connection
itself, which is why part 5 exists:

> After N consecutive session-open failures on the same instance, within a bounded window, treat the
> instance as unusable: drop the lease and provision a replacement. N and the window are configuration,
> and the counter resets on any successful session.

The threshold matters more than it looks: too eager and a brief network blip destroys a client's files;
too lazy and an operator waits out a ttl of hours. It should default to "clearly broken", not "slightly
slow".

**The general lesson, worth stating because it will recur:** a lease is a claim about *usability*, not
about *existence*. Every check in this codebase that asks "does the object exist" is answering a
strictly weaker question than the one that matters.

## Context

The truth about who holds which instance lives in two places: an in-process table
(`internal/lease`, `byIdentity`) and the `byre.io/identity` annotation on the instance object.
`Adopt` reconciles them at startup by listing annotated objects and rebuilding the table.

That reconciliation is a startup-only compensation, and it is the only thing keeping the two
copies honest. In steady state there is no channel from "the object is gone" to "the table still
points at it". `Alive()` is the single defence, and it runs on specific paths only.

**Observed failure (2026-11-20).** An operator deleted the sandboxes to force a reschedule. The
claim and the pod went away together, which leaves no intermediate state for `Alive()` to catch.
The table kept the dead addresses, so **every** new connection was dialled at a target that no
longer existed:

```
Session open refused by peer
Connection to byre.byre-system.svc.cluster.local closed by remote host
```

Every ssh-backed tool in the deployment failed — including the one the operator was using to
diagnose it — while the gateway process itself was healthy and its kanban tooling kept working.
The only recovery was `kubectl rollout restart deploy/byre`: kill the process to clear the table.

TTL does not help here. `Reap` walks the in-memory table, so a row whose instance is gone is
still a row; it is reaped only if its idle clock expires, and an entry that keeps being retried
keeps looking active.

**Observed failure 2 (same day, same root cause).** Two Hermes sessions for one identity. Hermes
opens *two* TCP connections per environment — one for commands (carrying `SendEnv` variables), one
for `scp`/sync over a plain socket. In a lease model each connection is a claim on an instance, and
the only arbiter is the in-memory table inside one process. The sync connection was bound to a
different instance than the command connection, so credentials and most skills were written into a
sandbox nobody was looking at while the terminal's sandbox had neither:

```
~/.hermes/skills:        4 files   (361 expected)
~/.hermes/credentials:   absent
```

Nothing in that path is random: it is the same defect as failure 1, seen from the other side. The
binding is not in a place both readers can see, so two acquisitions for one identity can disagree.
With the binding in a CR, the second acquisition either reads the first one's entry or loses a
`resourceVersion` race and re-reads it; there is no window in which both parties believe they won.

## Decision

1. **Binding state moves to a `ByreLease` custom resource.** `spec` carries what must be true for
   every reader: `identity`, the stable `name`, `backend`, `instance`, `instanceUID`. `status.phase`
   carries `Leased | Draining`. The API server is the truth; the in-process table becomes a cache.

2. **Only the binding is persisted.** Everything else stays in memory, because it is derived or
   local rather than shared:

   | State | Where | Why |
   |---|---|---|
   | identity → instance binding, `instanceUID` | CR spec | restarts, replicas and external observers must agree |
   | `phase` | CR status | arbitration; also the only way "instance gone, row stale" is visible |
   | connection count `refs` | memory | process-local by definition; two replicas count their own |
   | idle clock `lastSeen` | memory | recomputed conservatively after a restart |
   | pool warm / inflight | memory | pure local scheduling state |
   | metrics counters | memory | losing them costs nothing |

   As a result **the hot path performs no API writes at all**: `KeepAlive` touches memory only,
   and `acquire` / `release` are the only writers. There is deliberately no "flush lastSeen every
   30s" mechanism — that is only necessary if `lastSeen` were persisted, and it is not.

3. **`Adopt` is demoted to a migration tool.** It exists to fold existing
   `byre.io/identity` annotations into CRs once, then it can be deleted. Startup becomes
   `List` the leases; there is no "rebuild the table" step.

4. **Watch closes the failure mode.** A `Deleted` event on an instance object moves its lease to
   `Draining`, which is what makes the 2026-11-20 sequence impossible: no path depends on
   someone calling `Alive()` at the right moment.

## Consequences

- **Multi-replica becomes possible.** `resourceVersion` is a compare-and-swap, which is one of the
  two mechanisms ADR-0010 requires before `replicas: 2`. The other (leader election) is not needed.
- **New API surface**: a CRD, its RBAC, and a migration. The repository grows a `api/` package
  and the deployment needs `leases` permissions on the target namespace.
- **A second object to keep consistent**: deleting a lease must not orphan an instance, and deleting
  an instance must not leave a `Leased` lease. Both directions need a controller loop or a
  reconciliation on read.
- **Cost of being wrong is lower.** A stale CR is visible with `kubectl get`, which is a debugging
  surface the in-memory table never had.

## Alternatives considered

**Informer only (no CRD).** Watch instance objects and drop table rows on `Deleted`. This fixes the
observed failure with ~100 lines and no new API. It does not fix restarts (still `Adopt`), does not
give a second replica anything to read, and leaves two copies of the truth. Worth doing first if
the CRD is deferred, but it is a patch on the architecture rather than a correction of it.

**Redis (or another external store).** Works, but adds a dependency the project does not otherwise
need, and re-implements CAS that the API server already provides. Only justified if leases must
outlive the cluster they describe, which is not the case.

**Store `lastSeen` in the CR as well.** Rejected: it converts every SSH activity into an API write,
which is the one cost that would make this design worse than the status quo.

## Implementation status

| part | what | state |
|---|---|---|
| 1 | the `ByreLease` type and the store over it | merged |
| 2 | `lease.store` in the config, and the manager writing bindings | merged |
| 3 | startup recovery, plus the ownership boundary it turned out to require | merged |
| 4 | a watch that moves a lease to `Draining` when its object disappears | merged |
| 5 | connection-level degradation when an instance stops accepting sessions | not started |

Acceptance (1) belongs to part 4, not to part 3, and the distinction is worth being precise about:
recovery cleans a dead binding at startup and the reuse path cleans it on the next connection *from that
identity*, but "an operator reconnects without anybody restarting anything" needs the watch to notice the
deletion while the process runs. Acceptance (2) needs a second replica, which is not deployed; the
compare-and-swap is in place and unit-tested, but the fake client cannot reject a stale write, so that
one stays unverified until a replica exists to observe it. Acceptance (3) and (4) are checkable today.

### The boundary part 3 had to add

Adoption was never owner-scoped: `List` returns every annotated instance the credentials can see.
Recovery made that fatal, because a recovered lease is a lease the reaper will eventually release when its
ttl expires. Building part 3 on a real cluster, with credentials that could see a live deployment, was
enough to have that deployment's instances released -- they were temporary, so nothing durable was lost,
and that is luck rather than design.

`Manager` now takes the set of identities it serves and refuses to adopt anything else, and refuses to
delete rows for identities it does not serve. The tests cover the adoption path, the recovery path, and
-- the one that matters -- that a foreign instance survives a full ttl without being released, because
adoption and destruction are separated by a timer and a test that stops at "it was not adopted" would pass
while the instances disappear.

The general rule this is an instance of: **"an object I can see" is not "an object I own"**, and any code
that acts destructively on a listed object needs that distinction made explicit, because the credentials
decide what is visible and the configuration decides what is yours, and they are not the same set.

## Acceptance

Machine-checkable, in order:

1. Delete the `Sandbox` object behind an active lease → **without restarting byre**, the next
   connection from that identity obtains a new instance within `ttl`.
2. Run two replicas → two concurrent connections from one identity never receive two instances
   (the loser sees 409 and re-reads).
3. `kubectl get byreleases -l app.kubernetes.io/managed-by=byre` count equals the number of
   instances, with no duplicate `spec.identity`.
4. `byre_leases_adopted` disappears from `/metrics` (nothing to adopt), and `byre_lease_dead_instance_total`
   stays 0 across the delete in (1).
