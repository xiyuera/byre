# Limits and things known to be wrong

Written down rather than discovered in production. If one of these blocks you, that is worth an issue.

## Single replica

One process holds the lease table, so a second replica would give one identity two instances. The
multi-replica designs are written down in the [roadmap](roadmap.md); neither is implemented.

The lease table can be moved out of the process — `lease.store: crd` keeps it in a `ByreLease` object in
etcd, which is what makes a restart re-adopt live leases instead of starting clean. That is durability, not
concurrency: it is still one writer. The preconditions a second replica would have to satisfy are in
[ADR-0010](adr/0010-multi-replica-preconditions.md).

## `/healthz` does not mean "I can hand out an instance"

It answers "the process is up" and prints the version stamped into the binary. A gateway missing its
cross-namespace grant is green, never warms its pool, and refuses every checkout with a 403. This is the
single most misleading failure in the project, and it is why
[Operations](handbook/operations.md) puts the grant check before everything else.

## `provision: command` adopts only what it can attribute

A restart adopts the instances that `list` reports with `annotations.byre.io/identity` and keeps the rest
with a warning — a deliberate leak in place of a destructive cleanup, so an un-annotated lister leaves warm
capacity behind rather than destroying live leases. Printing the annotation in `acquire` but not in `list`
(or the reverse) gives an instance that is listed but never matched to the identity that reconnects.

## No automated end-to-end gate

The cluster runs in [acceptance runs](acceptance/index.md) were executed and recorded by hand. The harness is
`BYRE_CFG=config.yaml BYRE_KEY=<client key> test/e2e/run.sh [--ttl-ok]`; it asserts against a gateway that
is already running, and without those two variables it exits 2. A `kind`-based CI run is a roadmap item.

## `endpoints` does not survive a restart

That driver keeps ownership in memory, so a restarted gateway round-robins again and a client that
reconnects lands on whichever endpoint is next. `claims` and `workloads` do survive, because the identity is
written onto the object itself.

## `grace: 0s` does not mean "no grace"

`ApplyDefaults` replaces an explicit zero with 60 s, and there is no way to disable it. `pool_size: 0`, by
contrast, does mean what it says: no warm capacity.

## First connections can lose a race with an empty pool

With `claims` and a warm pool that has been drained, several identities connecting at once can outrun the
controller: the claim exists before the pod is `Ready`, and a connection arriving in that window is closed
(`Connection ... closed by remote host`). The lease is created anyway, so retrying a second later succeeds.
Raising `SandboxWarmPool.replicas` above your expected concurrent first connections avoids it.

## Node choice is not byre's decision

If your cluster has nodes of very different speed, an identity can land on a slow one and stay there for the
whole lease — byre does not reschedule. Measured on a mixed cluster: the same workload took 1.7 s on the
fast nodes and 10.5 s on the slow ones. Use `nodeSelector` or `affinity` on the `SandboxTemplate` if that
matters to you.

## Not in v1

Multi-replica consistency, dynamic backend balancing, in-instance state persistence, multi-tenancy, a UI.
Anything specific to one product belongs in an integration example, not in the gateway.
