# ADR-0014 — `list` reports every object it selected, including the ones it cannot dial

- Status: accepted
- Date: 2026-11-20
- Deciders: maintainers
- Corrects: the claim path of `Kubernetes.List`, and the assumption that adoption is enough cleanup

## Context

Measured on a live gateway (`kubectl -n hermes`), 2026-11-20 06:23–07:05 UTC:

| claim | `byre.io/identity` | sandbox | created (UTC) |
|---|---|---|---|
| byre-db05fc0d | `IFXHiuBwm3X5C/…` (default) | sandbox-lnp8g | 06:23:27 |
| byre-b11f7e8c | none | sandbox-hfns5 | 06:31:18 |
| byre-131237e2 | none | sandbox-47jgj | 06:53:51 |

The gateway restarted at 07:05:33. Its log says adoption discarded two objects for
`no identity annotation (stale warm capacity)` — `byre-0eb4523e` and `byre-a367b590` — and the two
claims above survived: Running, unowned, and invisible to every later restart. Its own `/metrics`
agrees with the log: `byre_instances_discarded 2`, `byre_leases_adopted 1`.

The reason they were not among the discarded is the claim branch of `List`:

```go
for i := range list.Items {
    item := &list.Items[i]
    if addr := k.endpointFromObject(item); addr != "" {   // <- only status.sandbox.serviceFQDN
        out = append(out, …)
    }
}
```

An object whose address was not published in that one field was dropped from the list. `Adopt` and
`Reap` both work from that list, so such an object was neither reused nor destroyed — it was simply not
there. Readiness had always accepted two sources (`endpointCandidates`: the configured status field,
then `status.sandbox.podIPs`), so an instance could be *ready* by one field and *invisible* by the other.

The second defect is what made it expensive to diagnose: every pod carried `hermes.profile: default`
from the SandboxTemplate, whatever identity held it. Two people read that as "the default profile is
running twice" and started looking in the wrong component.

## Decision

**`List` returns every object the selector matched, with or without an address.** "I cannot dial this"
and "this is not mine" are different statements, and only the caller can act on the first. The address
is read with the same preference order readiness uses, and an object that has none is logged once
(`managed object advertises no address`) and returned anyway.

The consequences downstream are deliberate:

- an anonymous object (no `byre.io/identity`) is stale capacity: adoption releases it at startup, as
  before — now including the ones that carry no address;
- a *bound* object with no address fails `Instance.Validate` and is released as unusable. That is
  correct rather than harsh: the driver only ever annotates an object **after** it observed an address
  answering on port 22, so an annotated object that advertises none has lost its sandbox, and the
  client's next connection would be handed an address nothing answers on.

**Adoption is not the only cleanup.** An object can be orphaned while the process runs: a create that
lost its annotate step, a release that failed three times. The reaper now sweeps as well, on its own
slower clock (every 5 minutes) and with an age guard (15 minutes, comfortably above any
`provider.ready_timeout`). It only touches drivers that implement `backend.StaleCapacity` and promise
that anonymous instances are stale, and it skips anything a lease row or the warm pool still references.
The age guard exists because a create that is still waiting for its endpoint has no ID yet and is
therefore invisible to the lease table; only age separates it from an abandoned object. A driver that
reports no creation time is never swept.

**Identity is written as a label as well as an annotation.** `byre.io/identity` keeps the exact
fingerprint (annotations allow the base64 characters a label value does not). Alongside it the driver
writes `byre.io/identity-ref` — the same value transliterated, so it is selectable — and whatever labels
the configuration declares per identity:

```yaml
identities:
  - fingerprint: "WcHWo47zy7eNKb8CgeTkYX4rjr4CoNcp+xtNo63NYRE"
    backend: sandbox
    aliases: ["eng"]
    labels:
      hermes.profile: eng
```

The last block is the answer to the misdiagnosis above, and it is configuration rather than code because
the label an operator's tooling filters on is not byre's to invent. Whether a *pod* shows it depends on
the workload's template: with `provision: claims`, the pod is built by the sandbox controller from a
SandboxTemplate, and its labels come from `spec.podTemplate.metadata` plus whatever the template
propagates from the object. byre cannot set them from the claim directly. So the template has to stop
hard-coding the constant (`hermes.profile: default`) and either propagate the claim's labels or drop the
key altogether — `docs/identity-on-the-pod.md` has both variants and the commands to check which one the
installed controller supports.

## Alternatives considered

- **Filter the list harder** (require both labels and a sandbox reference) — rejected: the failure mode
  being fixed is exactly the one where a filter hides an object from its only cleaner.
- **Destroy anonymous objects on a timer in the driver** — rejected: ownership policy belongs in the
  lease manager, which is the only component that knows what a lease is. The driver reports what it
  sees; the manager decides.
- **Write the identity onto the Sandbox or the pod** — rejected for now: it needs write access to
  objects byre does not own, and it races the controller that owns them. Labels on the claim plus a
  propagating template achieves the same result with the permissions byre already has.
- **Keep the constant `hermes.profile: default` and document it** — rejected: a label that is the same
  for every identity is worse than none, because it reads as an answer.

## Consequences

- `Instance` gained `Created`, which drivers fill from the object's `creationTimestamp`; the lease
  manager uses it only for the sweep's age guard.
- `byre_instances_reclaimed` is new, and `byre_instances_discarded` now counts both startup decisions and
  sweep decisions. A non-zero `reclaimed` after a long uptime means objects are being orphaned while the
  gateway runs — a defect in the create/bind/release path, not leftovers.
- `scripts/publish-image.sh` refuses to publish an image whose binary reports the unstamped default, so
  "the process says `version="dev"`" cannot quietly reach a release again.
- Reuse within the lease window is unchanged (`byre_acquire_total{result="reuse"}`); the sweep never
  looks at an instance a lease row references.
