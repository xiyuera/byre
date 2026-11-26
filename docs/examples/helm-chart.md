# byre Helm chart

The gateway plus, optionally, the sandbox template and warm pool it leases from.

```sh
helm install byre ./charts/byre -n byre --create-namespace
```

That installs and starts. It will not *do* anything yet, because two values have no safe
default: which keys may ask for a lease, and which key the gateway uses to log in to
instances. Both are deliberate — a chart that defaulted to "anyone" or shipped a key pair
would be a foot-gun.

## The two values you must set

```sh
# 1. the pool key: the gateway's identity towards instances
examples/docker-sandbox/scripts/keys.sh            # writes keys/id_key{,.pub}
helm upgrade byre ./charts/byre -n byre \
  --set-file poolKey.privateKey=examples/docker-sandbox/keys/id_key
# or, better, create the secret yourself and point at it:
#   --set poolKey.existingSecret=byre-pool-key

# 2. who may connect
ssh-keygen -lf ~/.ssh/id_ed25519.pub               # SHA256:...
helm upgrade byre ./charts/byre -n byre \
  --set identities[0].fingerprint=SHA256:... \
  --set identities[0].backend=sandbox
```

Then, from inside the cluster:

```sh
kubectl -n byre port-forward svc/byre 2222:2222
ssh -p 2222 -i ~/.ssh/id_ed25519 sandbox@127.0.0.1 hostname
```

## Where the lease table lives

`lease.store` picks the backing store:

| value | what it means |
|---|---|
| `crd` (default) | One `ByreLease` object per identity, in `lease.namespace`. Survives a gateway restart; readable with `kubectl get byl -n <ns>`; two gateways reading the same namespace agree on who holds what. |
| `memory` | Kept in the gateway process. A restart loses every lease (the instances stay allocated until their TTL), and more than one replica hands out independent instances for the same identity. |

The CRD (`byreleases.byre.io`, Namespaced, short name `byl`) ships in this chart's
[`crds/`](https://github.com/xiyuera/byre/blob/main/charts/byre/crds/byreleases.yaml), so a plain `helm install` creates it. Two consequences worth knowing
before you upgrade:

- **`helm upgrade` never updates CRDs.** Helm installs `crds/` on the first install and then leaves
  them alone, by design, because a CRD change can destroy data. If a newer chart version ships a newer
  CRD, apply it yourself:

  ```sh
  helm pull oci://ghcr.io/xiyuera/byre --untar   # or take crds/byreleases.yaml from the repo
  kubectl apply -f byre/crds/byreleases.yaml
  ```

- **`helm uninstall` does not delete the CRD** — so the `ByreLease` objects survive a reinstall.
  Remove it deliberately if you mean to: `kubectl delete crd byreleases.byre.io`.

If you cannot create cluster-scoped objects, install with `--skip-crds --set lease.store=memory`.
That combination is coherent (nothing reference the CRD), and it is what CI uses to render the chart
without a cluster.

When `lease.store=crd` the chart also renders a second Role and RoleBinding (`<fullname>-leases`) in
`lease.namespace`, granting `byreleases` and `byreleases/status`. Without it the store's readiness
check cannot list `byreleases` and the gateway falls back to reading identity off the instance
annotation — it looks healthy while holding no leases at all.

`lease.namespace` exists because the lease table and the sandboxes it describes need not live
together: one shared lease namespace with per-team sandbox namespaces is a reasonable layout. Empty
falls back to `backend.namespace`, and that empty falls back to the release namespace.

## Sandbox template and pool

`sandbox.enabled` is **false** by default. Turning it on renders a `SandboxTemplate` and a
`SandboxWarmPool`, and the template points at `sandbox.image` — if that image does not exist
yet the warm pods sit `Pending` and the gateway has nothing to hand out. Build one first:

```sh
docker build -f examples/docker-sandbox/Dockerfile.sandbox -t <reg>/byre-sandbox:latest examples/docker-sandbox
docker push <reg>/byre-sandbox:latest

helm upgrade byre ./charts/byre -n byre \
  --set sandbox.enabled=true \
  --set sandbox.image.repository=<reg>/byre-sandbox \
  --set sandbox.image.tag=latest \
  --set sandbox.warmPool.replicas=2
```

The controller that backs those CRDs (`extensions.agents.x-k8s.io/v1beta1`) is **not**
installed by this chart. Install it first:

```sh
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/latest/download/sandbox-with-extensions.yaml
kubectl -n agent-sandbox-system rollout status deploy/agent-sandbox-controller
```

Turning `sandbox.enabled=true` without it fails at install time with `no matches for kind
SandboxTemplate` — which is the honest failure, but it happens after the gateway Deployment is
already created, so the install half-succeeds. Apply the controller manifest first.
[The kubernetes starter kit](kubernetes-sandbox.md) has the walkthrough.

## Values that change the shape

| value | default | what it does |
|---|---|---|
| `backend.provision` | `claims` | `claims` \| `command` \| `workloads` \| `endpoints` |
| `backend.command.{acquire,release,list}` | `[]` | programs for the `command` provisioner |
| `backend.endpoints` | `[]` | static hosts for the `endpoints` provisioner |
| `backend.ssh.hostKeyPolicy` | `accept-new` | `strict` for long-lived hosts, `insecure` for throwaway containers |
| `lease.ttl` / `lease.grace` | `10m` / `30s` | idle reclaim |
| `lease.poolSize` | `null` | `0` means "no warm capacity, create on demand" |
| `lease.store` | `crd` | `crd` \| `memory` — see below |
| `lease.namespace` | `""` | namespace for `ByreLease` objects; empty = sandbox namespace = release namespace |
| `replicaCount` | `1` | >1 hands out independent sandboxes per identity; see the note in `values.yaml` |
| `rbac.create` | `true` | set false when you pre-bind a ServiceAccount |
| `sandbox.enabled` | `false` | render the template + warm pool |
| `extraVolumes` / `extraVolumeMounts` | `[]` | e.g. a PVC for the host key so it survives restarts |

Every value in `values.yaml` is documented inline; nothing is hard-coded in the templates.

## Why the host key is an emptyDir

Restarting regenerates it, which breaks any client that pinned the fingerprint. That is the
right default for a chart (no surprise storage class), not the right answer for production:
supply a PVC through `extraVolumes` and point `host_key` at it in the ConfigMap.
