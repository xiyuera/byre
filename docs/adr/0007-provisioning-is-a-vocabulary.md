# ADR-0007 — Provisioning is a vocabulary, not a product

- Status: accepted
- Date: 2026-11-17
- Deciders: maintainers

## Context

The first schema had `backend.type: kubernetes | static` and, inside the Kubernetes one,
`provider.kind: sandbox-claim | pod`. That reads as general and is not:

- `sandbox-claim` names **this deployment's** CRD (`SandboxClaim` in `extensions.agents.x-k8s.io`). A
  reader with a different controller — or no controller — has no idea whether their setup is supported,
  and a reader of the source learns a vocabulary from someone else's cluster.
- `type: kubernetes` makes Kubernetes the axis, so the honest answer to "how do I plug in a cloud VM" is
  "you can't", even though nothing in the lease logic cares where the instance lives.
- The two axes (type, kind) were orthogonal in the wrong way: `kind` decided behaviour while `type`
  decided the driver, and neither described the thing the gateway actually needs — *an SSH endpoint that
  can be created and destroyed*.

The question that exposed it: "k8s 上的 ssh 后端，或者其他的 ssh 后端应该以何种形式进入配置". There was
no good answer, which is the definition of a schema that is not yet general.

## Decision

One axis, four behaviours, named after what they do:

| `provision` | meaning | driver |
|---|---|---|
| `claims` | a controller hands one over when asked; the API coordinates are configuration | kubernetes |
| `workloads` | we create the workload from an image that runs sshd | kubernetes |
| `endpoints` | no provisioning: a fixed set of hosts that already exist | static |
| `command` | an external program provisions and describes the instance | command |

Everything about *reaching* an instance is shared and lives outside the provision block: `ssh.user`,
`ssh.private_key`, `ssh.host_key_policy`, and readiness timeouts. A backend that speaks SSH is
a backend, whatever created it. (`ssh.port` sits in that block too, but no driver reads it: the port is
part of the instance's `addr`, or of each `endpoints` entry.)

`claims` keeps its explicit API coordinates (`api_group`, `api_version`, `resource`, `object_kind`,
`endpoint_field`) — there is still no built-in API — but the *name of the provision* no longer implies
which one.

`command` is the escape hatch, and its contract is deliberately small: the program prints one JSON object
(`{"id","addr","user?","private_key?"}`), release runs another with `BYRE_INSTANCE_ID` in the environment,
and an optional `list` lets a restarted gateway adopt what it left running. That is the shape a cloud
provisioner, an Incus wrapper, or a CMDB query can satisfy in a shell script.

## Consequences

- A stale config fails with a sentence, not a schema error: `backend.type` is read only to be rejected
  with "was replaced by backend.provision".
- `command` without `list` cannot adopt after a restart. Orphans from that backend are the operator's
  to clean up, and the documentation says so at configuration time instead of at the first restart.
- `workloads` pods are created with no resources and no security context — the configuration does not yet
  express them. Tracked with the hardening card; it is a real gap, not a stylistic one.
- The provisioning interface stays a Go interface with four in-tree implementations. No plugin system:
  with four shapes and one consumer, a plugin boundary would be speculative infrastructure, and the
  `command` provision already covers "anything else".

## What would reverse this

- A fifth shape that cannot be expressed as one of the four (for example provisioning that needs to stream
  progress, or one that returns a fleet rather than an instance). At that point the interface — not the
  vocabulary — is wrong, and the change belongs in `backend.Backend`.
- Someone needing instance-level resource requests through `workloads`. That is a missing field, not a
  missing provision.
