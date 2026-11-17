# Documentation index

The repository [README](../README.md) is the front door: what byre is, a five-minute Quick start, and
how the four provisioning shapes differ. This page is the map for everything past that point.

## Handbook

| document | what it answers |
|---|---|
| [handbook/architecture.md](handbook/architecture.md) | how the pieces fit — identity, lease, pool, backends, readiness — and why the four obvious repairs fail |
| [handbook/backends.md](handbook/backends.md) | the four provisions in detail: which one to pick, every field, the readiness rule, failure behaviour, and the `command` contract |
| [handbook/operations.md](handbook/operations.md) | running it: first install, the grant that is not in the overlay, host-key rotation, metrics, flags, upgrading, uninstall |
| [handbook/troubleshooting.md](handbook/troubleshooting.md) | what each failure looks like from the client and from the gateway log, and what to check first |

## Runnable examples

| example | shape | what it is |
|---|---|---|
| [examples/local-quickstart/](../examples/local-quickstart/README.md) | `endpoints` | the README's Quick start as a file: one machine, no cluster |
| [examples/static-endpoints/](../examples/static-endpoints/README.md) | `endpoints` | a fixed set of lab hosts |
| [examples/pod-image/](../examples/pod-image/README.md) | `workloads` | a minimal sshd image and the backend that creates pods from it |
| [examples/kubernetes-sandbox/](../examples/kubernetes-sandbox/README.md) | `claims` | a template, a warm pool, and the backend that claims from them |

## Decisions and records

| document | what it answers |
|---|---|
| [adr/](adr/) | each decision, with the conditions under which it would be reversed |
| [acceptance/](acceptance/) | what was verified on a real cluster, with the commands and the outputs |
| [roadmap.md](roadmap.md) | what is deliberately absent from v1, in the order it would arrive |
| [release.md](release.md) | when to cut a version, what the pipeline produces |
| [integrations/](integrations/) | how a host runtime (agent platform, CI) talks to the gateway |
| [rename-to-byre.md](rename-to-byre.md) | the name history, the collisions, and the cutover that is still pending |
| [i18n.md](i18n.md) | which pages are translated, and how the checker decides a translation is stale |

Start with `handbook/architecture.md` if you are reading the code, `handbook/operations.md` if you are
running it, and `handbook/troubleshooting.md` if it is running and does not work.
