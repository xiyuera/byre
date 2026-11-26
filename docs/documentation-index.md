# Documentation index

The [home page](index.md) is the front door: what byre is, a five-minute Quick start, and how the four
provisioning shapes differ. This page is the map for everything past that point.

## Getting started

| page | what it answers |
|---|---|
| [Quick start](getting-started/quickstart.md) | the smallest working gateway, in five minutes |
| [Choose a backend](getting-started/backends.md) | which of the four provisioning shapes fits your hosts |
| [Kubernetes](getting-started/kubernetes.md) | sandboxes created on demand, end to end |

## Handbook

| page | what it answers |
|---|---|
| [Architecture](handbook/architecture.md) | how the pieces fit — identity, lease, pool, backends, readiness — and why the four obvious repairs fail |
| [Backends](handbook/backends.md) | the four provisions in detail: which one to pick, every field, the readiness rule, failure behaviour, and the `command` contract |
| [Operations](handbook/operations.md) | running it: first install, the grant that is not in the overlay, host-key rotation, metrics, flags, upgrading, uninstall |
| [Troubleshooting](handbook/troubleshooting.md) | what each failure looks like from the client and from the gateway log, and what to check first |

## Examples

| example | shape | what it is |
|---|---|---|
| [Static endpoints](examples/static-endpoints.md) | `endpoints` | a fixed set of lab hosts, leased one identity at a time |
| [Local quickstart](examples/local-quickstart.md) | `endpoints` | the Quick start as a file: one machine, no cluster |
| [Any image that speaks SSH](examples/pod-image.md) | `workloads` | a minimal sshd image and the backend that creates pods from it |
| [Kubernetes starter kit](examples/kubernetes-sandbox.md) | `claims` | a template, a warm pool, the backend that claims from them, and the RBAC + deployment to run it |
| [Docker starter kit](examples/docker-sandbox.md) | `command` | the whole gateway in one process, sandboxes as docker containers — no cluster at all |
| [Helm chart](examples/helm-chart.md) | `claims` | the Kubernetes shape as a chart, CRD and warm pool included |

## Design decisions

| page | what it answers |
|---|---|
| [Decision records](adr/index.md) | each decision, with the conditions under which it would be reversed |
| [Roadmap](roadmap.md) | what is deliberately absent from v1, in the order it would arrive |
| [Limits](limits.md) | what is known to be wrong or absent, stated plainly |
| [Integrations](integrations/README.md) | how a host runtime (agent platform, CI) talks to the gateway |

## Contributing

[Contributing](contributing.md) covers the branch and the merge: what to run before opening a pull
request, and what the release process expects of a change.

Start with [Architecture](handbook/architecture.md) if you are reading the code,
[Operations](handbook/operations.md) if you are running it, and
[Troubleshooting](handbook/troubleshooting.md) if it is running and does not work.

## Translation status

The home page, Quick start, Kubernetes, Architecture, Limits, Why not a jump host and this page have a
Chinese version under `/zh/`. Everything else falls back to English rather than 404ing — a page that
exists in one language is never a broken link in the other.
