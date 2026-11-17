# Documentation index

The repository [README](../README.md) is the front door: what byre is, a five-minute Quick start, and
how the four provisioning shapes differ. This page is the map for everything past that point.

## Handbook

| document | what it answers |
|---|---|
| [handbook/architecture.md](handbook/architecture.md) | how the pieces fit — identity, lease, pool, backends, readiness — and why the four obvious repairs fail |
| [handbook/backends.md](handbook/backends.md) | the four provisions in detail: every field, the readiness rule, failure behaviour, and how to write a `command` backend |
| [handbook/operations.md](handbook/operations.md) | running it: first install, the grant that is not in the overlay, host-key rotation, metrics, upgrading, uninstall |
| [handbook/troubleshooting.md](handbook/troubleshooting.md) | what each failure looks like from the client and from the gateway log, and what to check first |

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
