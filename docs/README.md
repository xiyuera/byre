# Documentation index

| document | what it answers |
|---|---|
| [architecture.md](architecture.md) | how the pieces fit: identity, lease, pool, backends, readiness |
| [operations.md](operations.md) | running it: config, first install, the grant that is not in the overlay, rotation, symptoms |
| [release.md](release.md) | when to cut a version, what the pipeline produces, how GitHub is mirrored |
| [roadmap.md](roadmap.md) | what is deliberately not in v1, in the order it would arrive |
| [adr/](adr/) | decisions, each with the conditions under which it would be reversed |
| [acceptance/](acceptance/) | what was verified on a real cluster, with the commands and outputs |
| [integrations/](integrations/) | how a host runtime (agent platform, CI) talks to the gateway |
| [rename-to-byre.md](rename-to-byre.md) | the name history, the collisions, and the cutover that is still pending |

Start with `architecture.md` if you are reading the code, and `operations.md` if you are running it.
