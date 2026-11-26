# Documentation index

The site [home page](index.md) is the front door: what byre is, a five-minute Quick start, and how the
four provisioning shapes differ. This page is the map for everything past that point.

## Handbook

| document | what it answers |
|---|---|
| [handbook/architecture.md](handbook/architecture.md) | how the pieces fit — identity, lease, pool, backends, readiness — and why the four obvious repairs fail |
| [handbook/backends.md](handbook/backends.md) | the four provisions in detail: which one to pick, every field, the readiness rule, failure behaviour, and the `command` contract |
| [handbook/operations.md](handbook/operations.md) | running it: first install, the grant that is not in the overlay, host-key rotation, metrics, flags, upgrading, uninstall |
| [handbook/troubleshooting.md](handbook/troubleshooting.md) | what each failure looks like from the client and from the gateway log, and what to check first |

## Runnable examples

These are files in the repository, not pages of this site: each link below leaves the site for GitHub.

| example | shape | what it is |
|---|---|---|
| [examples/local-quickstart/](https://github.com/xiyuera/byre/blob/main/examples/local-quickstart/README.md) (in the repo) | `endpoints` | the Quick start as a file: one machine, no cluster |
| [examples/static-endpoints/](https://github.com/xiyuera/byre/blob/main/examples/static-endpoints/README.md) (in the repo) | `endpoints` | a fixed set of lab hosts |
| [examples/pod-image/](https://github.com/xiyuera/byre/blob/main/examples/pod-image/README.md) (in the repo) | `workloads` | a minimal sshd image and the backend that creates pods from it |
| [examples/kubernetes-sandbox/](https://github.com/xiyuera/byre/blob/main/examples/kubernetes-sandbox/README.md) (in the repo) | `claims` | a template, a warm pool, the backend that claims from them, and the RBAC + deployment to run it |
| [examples/docker-sandbox/](https://github.com/xiyuera/byre/blob/main/examples/docker-sandbox/README.md) (in the repo) | `command` | the whole gateway in one process, sandboxes as docker containers — no cluster at all |
| [charts/byre/](https://github.com/xiyuera/byre/blob/main/charts/byre/README.md) (in the repo) | `claims` | the Kubernetes shape as a Helm chart, template and warm pool included |

## Decisions and records

| document | what it answers |
|---|---|
| [adr/](adr/index.md) | each decision, with the conditions under which it would be reversed |
| [acceptance/](acceptance/index.md) | what was verified on a real cluster, with the commands and the outputs |
| [roadmap.md](roadmap.md) | what is deliberately absent from v1, in the order it would arrive |
| [release.md](release.md) | when to cut a version, what the pipeline produces |
| [tag-map.md](tag-map.md) | what each tag points at after the history rewrite, and the two published commit hashes that no longer exist |
| [contributing-from-agents.md](contributing-from-agents.md) | the account-per-role model, credential handling and token discipline for agent contributors |
| [integrations/](integrations/README.md) | how a host runtime (agent platform, CI) talks to the gateway |
| [rename-to-byre.md](rename-to-byre.md) | the name history, the collisions, and the cutover that is still pending |
| [i18n.md](i18n.md) | which pages are translated, and how the checker decides a translation is stale |

Start with `handbook/architecture.md` if you are reading the code, `handbook/operations.md` if you are
running it, and `handbook/troubleshooting.md` if it is running and does not work.

The home page, Quick start, Kubernetes, Architecture, Limits and Why not a jump host have a Chinese version
on the site, as does this page. Everything else — `handbook/backends.md`, `handbook/operations.md`,
`handbook/troubleshooting.md`, `release.md`, `i18n.md`, `integrations/` and four of the five examples — is
English only today and falls back to English under `/zh/`; [`i18n.md`](i18n.md) is the list, and
`scripts/check-i18n.sh` is how it stays visible.
