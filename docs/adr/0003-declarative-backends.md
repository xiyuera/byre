# ADR-0003 — Backends are declared in configuration, not compiled in

- Status: accepted; the schema in this record was superseded by ADR-0007
- Date: 2026-11-17
- Deciders: maintainers

## Context

The gateway needs three things from the outside world, and nothing more:

1. **obtain** an instance (or find an existing one),
2. **reach** it over SSH,
3. **return** it (destroy, or hand back to a pool).

Everything else — what an instance runs, how it is scheduled, whether it is a Kubernetes object at all —
is someone else's problem. Encoding one particular orchestrator into the gateway would make it useless
for the next deployment that has containers, VMs, or plain machines.

There is a second, organisational reason: the project must not carry one environment's specifics
(namespace names, template names, key paths, image tags) in its source, examples or docs. Those belong
in a configuration file the operator writes.

## Decision

A `Backend` is a declarative description of "what an SSH target is and how it comes into existence":

```yaml
backends:
  - name: <label>            # referenced by identities
    type: kubernetes|static
    namespace: <ns>          # type: kubernetes
    provider:
      kind: sandbox-claim|pod
      template: <name>       # kind: sandbox-claim
      warm_pool: <name>      # kind: sandbox-claim, optional
      image: <ref>           # kind: pod — any image that runs sshd is acceptable
      start_command: [...]   # kind: pod, optional
      ready_timeout: 120s
    ssh: { port, user, private_key, host_key_policy }
```

This is the schema as decided in this record. ADR-0007 later replaced `type` + `provider.kind` with a
single `provision` axis and renamed two of the claim fields; the decision in *this* record — that a
backend is declared in configuration and nothing environment-specific lives in the source — still
stands.

Design rules that follow from this:

- `type` selects the *driver*; `provider.kind` selects *how the instance is created*. Adding a driver
  (docker, remote cluster, a REST API) means adding a package, not editing the core.
- `ssh` is the only thing the core needs to know about the instance. A backend that supplies endpoints
  and a key is fully functional — the `static` type exists to prove that and to serve long-lived hosts.
- `image` + `start_command` is the extension point for "any image that speaks SSH": no code change is
  required to switch the workload image.
- The mapping `identity → backend` is data (`identities:`), not code, and can later come from a
  ConfigMap, a CRD or an HTTP source without touching the lease manager.

Validation is strict and happens before anything binds a port: unknown fields (`KnownFields`), unknown
`type`/`kind`, unknown backend references, duplicate fingerprints, malformed endpoints, out-of-range
ports and missing required fields all abort startup. A typo must never degrade into "no backends, so
everything is denied" or "wrong backend, so the wrong image runs".

## Consequences

- The core package knows two interfaces (`Backend`: acquire/release/list/adopt) and no orchestrator
  types. Unit tests use a fake backend; e2e uses the Kubernetes one.
- Operators can switch workload images and namespaces without a rebuild; that also means a bad config
  is a real risk, hence fail-fast validation and a `-check-config` mode for CI/deploy gates.
- Multi-backend selection stays static in v1 (`identities[].backend`). Dynamic placement is a roadmap
  item, and it can be added behind the same interface without changing the config shape.

## Alternatives rejected

| alternative | why not |
|---|---|
| hardcode one orchestrator (the reference deployment) | the tool would only serve one environment and leak its details |
| CLI flags instead of a file | many backends/identities, secrets paths, pool parameters; flags do not scale and leak into `ps` |
| per-backend plugins loaded at runtime (Go plugins) | fragile across builds, hard to test, no benefit over a driver package |
| allow unknown fields for forward compatibility | silently ignoring `templete:` typos is worse than failing |
