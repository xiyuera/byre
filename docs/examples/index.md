# Examples

Six runnable shapes, one per provisioning model plus the end-to-end starter kits. Each page is the
complete recipe for that shape — the config, what the host has to provide, and the failure modes that
are specific to it.

| shape | example | what it gets you |
|---|---|---|
| `endpoints` | [Static endpoints](static-endpoints.md) | Machines that are always on: lab boxes, build runners. The gateway leases them and hands them back; it never creates or destroys anything. |
| `endpoints` | [Local quickstart](local-quickstart.md) | The Quick start as a file — one machine, `localhost:22`, no cluster. The smallest thing that works. |
| `workloads` | [Any image that speaks SSH](pod-image.md) | A minimal sshd image, and the backend that turns it into pods on demand. |
| `claims` | [Kubernetes starter kit](kubernetes-sandbox.md) | The full cluster shape: template, warm pool, the backend that claims from them, RBAC and deployment. |
| `command` | [Docker starter kit](docker-sandbox.md) | The whole gateway in one process, sandboxes as docker containers. No cluster at all. |
| `claims` | [Helm chart](helm-chart.md) | The same Kubernetes shape as a chart, CRD and warm pool included. |

New to this? Start with [Local quickstart](local-quickstart.md) — it takes a minute and shows the lease
behaviour end to end. If you already have a cluster and want sandboxes on demand, go straight to the
[Kubernetes starter kit](kubernetes-sandbox.md).

The files each example refers to (templates, RBAC manifests, docker scripts) live in the repository under
`examples/` and `charts/`; the pages here explain them, the repository holds them.
