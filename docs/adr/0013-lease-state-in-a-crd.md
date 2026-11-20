# ADR-0013 — Lease state belongs in a CRD; memory holds only what is derived

- Status: accepted (implementation deferred to the next minor release)
- Date: 2026-11-20
- Deciders: maintainers
- Qualifies: ADR-0002 (one live lease per identity), ADR-0010 (multi-replica preconditions)

## Implementation note

Accepted as the direction; the code change is deliberately deferred to the next minor release, because
it needs a new API type, RBAC and a migration of the existing annotations.

If the failure recurs before then, the first-layer fix ships on its own: an informer watch that drops a
`byIdentity` row when its instance object is deleted. That is roughly 100 lines, moves no state, and
removes the "delete a sandbox, every entry point dials a dead address" outcome. It does not fix restarts
or enable a second replica — those still wait for this ADR.

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
