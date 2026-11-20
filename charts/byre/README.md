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
installed by this chart — see [the kubernetes starter kit](../../examples/kubernetes-sandbox)
for what it needs to provide.

## Values that change the shape

| value | default | what it does |
|---|---|---|
| `backend.provision` | `claims` | `claims` \| `command` \| `workloads` \| `endpoints` |
| `backend.command.{acquire,release,list}` | `[]` | programs for the `command` provisioner |
| `backend.endpoints` | `[]` | static hosts for the `endpoints` provisioner |
| `backend.ssh.hostKeyPolicy` | `accept-new` | `strict` for long-lived hosts, `insecure` for throwaway containers |
| `lease.ttl` / `lease.grace` | `10m` / `30s` | idle reclaim |
| `lease.poolSize` | `null` | `0` means "no warm capacity, create on demand" |
| `replicaCount` | `1` | >1 hands out independent sandboxes per identity; see the note in `values.yaml` |
| `rbac.create` | `true` | set false when you pre-bind a ServiceAccount |
| `sandbox.enabled` | `false` | render the template + warm pool |
| `extraVolumes` / `extraVolumeMounts` | `[]` | e.g. a PVC for the host key so it survives restarts |

Every value in `values.yaml` is documented inline; nothing is hard-coded in the templates.

## Why the host key is an emptyDir

Restarting regenerates it, which breaks any client that pinned the fingerprint. That is the
right default for a chart (no surprise storage class), not the right answer for production:
supply a PVC through `extraVolumes` and point `host_key` at it in the ConfigMap.
