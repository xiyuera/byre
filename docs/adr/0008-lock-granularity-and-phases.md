# ADR-0008 — The lease table is locked briefly, and a row says which phase it is in

- Status: accepted
- Date: 2026-11-17
- Deciders: maintainers
- Corrects: the implementation the previous version of `docs/architecture.md` §3 described

## Context

Two defects in `internal/lease/manager.go` were measured, not suspected, before this decision:

1. `Acquire` held the manager's single mutex across two backend round trips (`drv.Acquire` and, for a
   pooled instance, `binder.Bind`). A probe that made one identity's `Acquire` take 500ms measured
   **379ms** for `Lookup` of a *different* identity, and every other method (`KeepAlive`, `ReleaseConn`,
   `Stats`, `Reap`, `/metrics`) was blocked for the same span. A cold start is exactly when these calls
   are slow, so "the gateway is unavailable while it is starting" was the normal case, not the accident.
2. `Reap` deleted the table row under the lock and called `drv.Release` after unlocking. In that window
   the row was gone but the instance was still owned by the identity at the backend, so a reconnect was
   handed the same instance — and the release that followed destroyed it. A probe reproduced two
   identities sharing one instance (`inst-1`, `owner=map[]`).

Both are lifecycle problems: "the row exists", "the instance exists" and "the backend agrees who owns it"
are three facts that a backend call separates in time.

## Decision

**One mutex, held only for map and field updates. Every backend call happens outside it.** Rows carry the
phase they are in, and a row that is transitioning is something other goroutines wait on instead of
looking past:

| phase | meaning | `Acquire` for the same identity | `Lookup`/`Stats` |
|---|---|---|---|
| `pending` | an instance is being created for this identity | waits for the outcome, then re-reads the table | not a lease: `nil` / not counted |
| `leased` | usable, referencable | joins it (`refs++`) | visible |
| `draining` | the backend is destroying the instance | waits for the teardown, then creates a new one | not a lease |

- `Acquire` installs the `pending` row *before* calling the backend (refs recorded on completion), so a
  second connection for the same identity joins the in-flight creation instead of creating a second
  instance. The wait is on a per-row channel (`done`), never on the manager mutex.
- `Reap` marks the expired row `draining` under the lock and releases outside it. The reconnect that used
  to be handed the dying instance now waits one backend call and gets a fresh one.
- Every path that destroys an instance — reap, invalidation, a failed `Acquire`, an adoption discard —
  removes the row **only if the table still points at that very row**, so a release can never delete a
  lease that was created while it was in flight. `backend.Static` already applied the same rule to its
  own ownership map (`Release` only clears the holder it recorded).
- `Manager.Invalidate` exists because a failed dial is a fact only the front end has: without it, a dead
  instance is handed to every reconnect until `ttl+grace` expires even though healthy capacity is idle.

## Alternatives considered

**Per-identity bucket locks.** This is what `architecture.md` promised. Rejected because it needs a
registry of locks with reference counting (to remove them without a race), it leaves two structures to
keep consistent, and it still leaves the reap/invalidate window open — the row would be reusable as soon
as the lock is dropped, unless the row also carries a phase. The phase is the part that fixes the
invariant; the lock granularity only fixes latency. Given `pending` rows, the global mutex is held for a
map write and a handful of field updates, and the measured 379ms becomes microseconds.

**Optimistic re-read instead of a phase.** ("Release, then check whether anyone took it.") Rejected: it
still hands the instance to the reconnect for a moment, and the client can open a session in that moment.

## Consequences

- A reconnect that lands in the teardown window waits for one `Release` call instead of being given a
  doomed instance. That is visible in latency of exactly one connection, not in correctness.
- Two connections for one identity now serialise on the creation of their shared instance. They already
  did — the lease forbids two instances — but it now costs nothing for other identities.
- With `pool_size > 0`, refills are still started after the checkout, and the reaper still releases per
  instance; both were already outside the table lock in spirit, and are now outside it in code.
- The reasoning above is single-replica reasoning. Multi-replica needs `resourceVersion` CAS on the
  annotation (ADR-0002 and `docs/roadmap.md`); nothing here changes that prerequisite.
