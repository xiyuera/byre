# ADR-0009 — `list` exists to adopt; "no annotation" means "unknown owner", not "stale"

- Status: accepted
- Date: 2026-11-17
- Deciders: maintainers

## Context

`list` is the only mechanism a restarted gateway has for finding the instances it was leasing when it
died. The code assumed it was also a garbage collector for warm capacity: `Manager.Adopt` released every
instance the lister reported without a `byre.io/identity` annotation. For the two drivers that write
that annotation themselves (`kubernetes`, `endpoints`) the assumption holds — an unannotated instance is
capacity nobody holds.

For `provision: command` it is false, and the failure is silent. Measured on a real gateway (v0.2.0):

```
$ cat release.env
BYRE_INSTANCE_ADDR=127.0.0.1:22022
BYRE_INSTANCE_ID=vm-adopted
```

Two independent defects produced it:

1. `commandInstance` had no `annotations` field, so an `annotations.byre.io/identity` key in the
   lister's JSON was dropped during unmarshalling — the manager never saw it.
2. `Manager.Adopt` released everything without the annotation, so "the lister did not report an owner"
   and "nobody owns this" were treated as the same statement.

Together: **one restart destroyed every instance the lister knew about, including the one an agent was
connected to.** With the annotation present in the JSON and dropped by the parser, even a lister written
against the current contract lost its instances. The log said nothing — a `_ =` discarded the release
error, and the adoption count was only printed when it was non-zero.

## Decision

`list` means *adopt what belongs to somebody*. An instance is adopted when it carries
`byre.io/identity`; nothing else is inferred from a missing annotation by default.

The command-backend contract gains an optional field, in both `acquire` and `list` answers:

```json
{"id": "vm-17", "addr": "192.0.2.17:22", "annotations": {"byre.io/identity": "<fingerprint>"}}
```

Release-at-startup survives as an **explicit, per-driver promise** instead of an assumption. A driver may
implement `backend.StaleCapacity` to state "every instance I lease out carries an identity, so an
unannotated one is mine and stale":

| driver | `AnonymousInstancesAreStale` | what a restart does with an unannotated instance |
|---|---|---|
| `kubernetes` | true | releases it; the pool refills immediately |
| `endpoints` | true | releases it, which is a no-op: this driver owns no objects, only an in-memory owner map that starts empty after a restart |
| `command` | not implemented | **keeps it and logs a warning** |

A driver that makes no promise cannot lose instances to a restart, which is the whole point: destroying
something in use is worse than leaving warm capacity behind, because only the first is unrecoverable.

Observability is part of the decision, not decoration. Every startup decision is now one line:

```json
{"msg":"released instance at startup","instance_id":"byre-7f2c","backend":"pooled","reason":"no byre.io/identity annotation and the driver reports anonymous instances as stale capacity"}
{"msg":"instance kept at startup","instance_id":"vm-anon","backend":"cloud","reason":"no byre.io/identity annotation and the driver cannot promise that anonymous instances are stale; destroying it could kill a live lease"}
{"msg":"adoption finished","adopted":1}
```

`adoption finished` is printed even when it adopted nothing, because "found nothing" and "destroyed
something" used to look identical in the log.

## Alternatives considered

**B — a `BYRE_OWNER` variable (or a timestamp) to tell "created by this process, unannotated" from
"unknown owner".** Rejected as the primary fix. It keeps `list` unable to adopt, which is the reason
`list` exists at all; it adds a second source of truth for ownership (an env var next to the annotation)
that has to be kept consistent; and "created by an earlier run of the same process" is exactly the case a
restart is in, so the variable would have to be stable across restarts — at which point it *is* an
identity annotation with a worse name. The safety property B was reaching for is kept, but as the
fallback for drivers that cannot promise anything (the `command` row above) rather than as the model.

**Release nothing, ever.** Rejected: leftover warm capacity in the claims/workloads drivers would never
be collected, and the pool would grow a ghost instance per restart.

**Make the annotation mandatory in the command contract.** Rejected: it breaks every existing lister at
upgrade time, and a lister cannot invent an identity it was never told — `acquire` receives
`BYRE_IDENTITY`, but `list` is called with no environment at all. The graceful path is: keep working,
print annotations when you can.

## Consequences

- A `command` lister written before this ADR keeps working and is no longer destructive. It gains
  adoption only when it starts printing `annotations.byre.io/identity` in *both* `acquire` and `list` —
  printing it in only one of the two produces an instance that can be listed but never matched by the
  identity that reconnects.
- Unannotated instances of a command backend are now **not** reclaimed automatically. That is a
  deliberate leak over a destructive cleanup; the operator removes them in their own system, or upgrades
  the lister. The `instance kept at startup` warning is the only signal, and it names the instance.
- The kubernetes and endpoints drivers keep their previous behaviour, and now say so in one method each
  rather than by accident. A future driver has to decide explicitly.
- `Manager.Adopt` releases outside the lease table lock; a slow backend no longer stalls `Lookup` for
  every live session while startup cleanup runs.
- Failed releases at startup are returned to the caller and logged with the instance id, instead of being
  discarded by `_ =`.
