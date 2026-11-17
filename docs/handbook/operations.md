# Operations

This is the runbook: it assumes you have read the README's Quick start and now want to run byre for real.
Field-level detail for each backend is in [backends.md](backends.md), and the failure-by-failure guide is
[troubleshooting.md](troubleshooting.md).

## Deploy

```sh
make image-push IMAGE_REGISTRY=<registry>/<team>   # pushes the tag, prints no digest
podman push --digestfile /tmp/digest <registry>/<team>/byre:<tag>   # or read it back with docker inspect
# fill the image reference in deploy/ (see below), then:
kubectl -n byre-system create secret generic byre-pool-key --from-file=id_key=/path/to/key
kubectl apply -k deploy/
kubectl -n byre-system rollout status deploy/byre
```

The image reference in `deploy/deployment.yaml` is a **placeholder** (`<registry>/byre@sha256:<digest>`),
not a working default: this project has no registry of its own, and a bare name (`byre`) sends kubelet to
Docker Hub for `library/byre` — an ImagePullBackOff that reads like a broken manifest. Fill it in either
with an overlay whose `images:` block names the reference **exactly as written in the manifest**
(kustomize matches the string literally, because `sha256:<digest>` is not a digest it can parse), or with
`kubectl -n byre-system set image deploy/byre byre=<registry>/byre@sha256:<digest>`.

Pin by **digest**, not by tag. A tag is a mutable pointer: the same tag can be re-pushed with different
content, and with `imagePullPolicy: IfNotPresent` the node keeps its cached layers, so the Deployment
reports the new tag while the process is still the old build. A digest is a content hash — if the content
changes, the value changes, and a pull cannot silently answer with something else. `/healthz` prints the
version compiled into the binary, which is how you tell which build is actually running.

**The pool key Secret is deliberately not part of `kubectl apply -k deploy/`** — creating it from a file
here would put a private key in git. Create it before the first rollout, in the same namespace:

```sh
kubectl -n byre-system create secret generic byre-pool-key --from-file=id_key=/path/to/pool_key
```

Skipping it does not fail loudly: the Deployment's volume on that Secret is not optional, so the Pod sits
in `CreateContainerConfigError` and never starts. That is the intended failure mode — the alternative
(making the volume optional) lets the gateway start with no key and fail every acquire with a misleading
SSH error instead.

`deploy/` creates: ServiceAccount, Role/RoleBinding (in the target namespace), Deployment (SSH front end
on 2222, health on 8099), Service (ClusterIP 2222). The configuration comes from a ConfigMap; the pool
key comes from the Secret above. Both are examples — copy, then edit.

## Image registries: two tiers

Two registries serve this cluster and they are not interchangeable. Below, `<your-gitea-host>:<port>`
is your Gitea instance's container registry endpoint — the one releases are published to — and
`<internal-registry-host>:<port>` is a throwaway registry that lives on the cluster network.

| registry | role | auth | what belongs there |
|---|---|---|---|
| `<your-gitea-host>:<port>` (Gitea container registry) | release source of truth | token — `write:package` to push, `read:package` to pull | anything a workload runs: versioned tags, digests, traceable to a repository |
| `<internal-registry-host>:<port>` (`registry:2`, self-signed CA) | scratch space | none — anonymous push is allowed | one-off experiments, manual builds, images nobody will need next month |

The rule is one line: **anything that reaches production comes from the Gitea registry**. An image in
the scratch registry is temporary by default and may be deleted without notice. The scratch registry
is kept rather than retired because it already speaks the v2 API, which is cheaper than teaching
everyone `docker save` / `ctr import` for the occasional hand-built image.

### Repository names differ

Gitea requires `owner/image` — two path segments, where `owner` is a user or an organisation, and the
image name is independent of any repository name. The migration therefore renamed single-segment
repositories:

| scratch registry | Gitea |
|---|---|
| `byre` | `byre/byre` |
| `gitea/runner:6.0.0` | `byre/runner:6.0.0` |
| `hermes-sandbox-desktop:ssh-uid10000-v19` | `byre/sandbox-desktop:ssh-uid10000-v19` |
| `library/ci-go:1.23` | `byre/ci-go:1.23` |
| `library/golang:1.23-alpine` | `byre/golang:1.23-alpine` |

Container packages are **not** linked to a repository (`repo_id = 0`): they appear under the
organisation's packages page (`/byre/-/packages/container/<image>`), and the repository's Packages tab
stays empty. Deleting a repository does not delete its images, and deleting an image is an
organisation-level action.

### Pulling from Gitea inside the cluster

Nothing on the nodes has to change. The registry presents a Let's Encrypt certificate for
`<your-domain>`, which every node already trusts (`openssl s_client -verify_return_error` returns 0 on
every node), so `registries.yaml` needs no CA entry for it — and therefore **no `systemctl restart k3s`**.
The existing entry for the scratch registry stays because that registry is self-signed.

This is deliberate: a credential inside `registries.yaml` is a credential on every node's disk. Pull
credentials belong in a `kubernetes.io/dockerconfigjson` Secret instead, which is what
`imagePullSecrets` is for:

```sh
kubectl -n <namespace> create secret docker-registry gitea-registry \
  --docker-server=<your-gitea-host>:<port> \
  --docker-username=<gitea user> \
  --docker-password=<token with read:package only>
```

Then reference it from the workload (`spec.imagePullSecrets`) or, for every pod in a namespace, from
the default ServiceAccount. Three namespaces need it: `byre-system`, `hermes` (CI runner, sandbox
templates) and `agent-sandbox-system`.

The pull token is a dedicated one: `read:package` and nothing else. A token that can push lets its
holder replace what production runs, and there is no reason for a node to hold one.

### Proving a pull actually went to Gitea

A `Successfully pulled image` event is not proof: when the same digest is already present locally — the
scratch registry and Gitea hold byte-identical blobs — containerd downloads nothing, and the node may
even report the old repository name in `imageID`. The honest check uses an image whose layers exist
**only** in Gitea:

```sh
kubectl -n hermes run pullprobe --image=<your-gitea-host>:<port>/byre/pullprobe:v1 \
  --overrides='{"spec":{"imagePullSecrets":[{"name":"gitea-registry"}],"nodeName":"<node>","restartPolicy":"Never"}}'
kubectl -n hermes get pod pullprobe -o jsonpath='{.status.containerStatuses[0].imageID}'
```

`byre/pullprobe:v1` is a 162-byte layer kept for exactly this purpose. The container has no shell, so a
pod that ends in `Failed` (exec `/bin/sh`) is expected; what matters is `imageID` naming
`<your-gitea-host>:<port>` after a non-zero download. Delete the pod when done.

## Build and publish the image

```sh
make image                                        # byre:<git describe>; lands in the local daemon only
make image-push IMAGE_REGISTRY=<registry>/<team>  # pushes <registry>/<team>/byre:<tag>; see the digest below
# then fill the placeholder in both places — they are matched as a string, see deploy/kustomization.yaml:
#   deploy/deployment.yaml:39          image: <registry>/byre@sha256:<digest>
#   deploy/kustomization.yaml images:  name: <registry>/byre@sha256:<digest>, newName: …, digest: …
```

Two steps above are not optional and both fail in ways that do not mention them. `make image` only feeds
the local container runtime, and the manifest ships `<registry>/byre@sha256:<digest>` — a placeholder that
kubelet cannot resolve, so an unfilled `deploy/` stops in `ImagePullBackOff`. A missing `byre-pool-key`
Secret is worse: the volume is not `optional` (`deploy/deployment.yaml:77-80`), so the pod never starts
and sits in `CreateContainerConfigError`.

The image is a static binary on Alpine; nothing else is installed. The image's own user is `10001:10001`,
while the shipped Deployment runs the container as uid `65532` with `fsGroup: 10001`
(`deploy/deployment.yaml:23,27`). The `fsGroup` is what makes the `0400` Secret mount readable — the
ownership, not the uid, decides — and that pairing was verified on a real cluster in
`docs/acceptance/2026-11-17-deploy.md`. The two numbers should be the same; until the manifest is unified
on `10001`, describe the container by what the image says and by the `fsGroup` that makes it work.

The gateway's **own** SSH host key lives on the `byre-hostkey` PVC (`host_key:
/var/lib/byre/hostkey/id_ed25519`). On first start the file does not exist and the gateway creates it,
logging the fingerprint (`generated a new host key and persisted it`). Distribute that fingerprint to
clients; from then on restarts keep the same identity.

To supply your own key instead, mount a Secret at that path — an existing file is never rewritten.

Rotation (deliberate):

```sh
kubectl -n byre-system scale deploy/byre --replicas=0
kubectl -n byre-system exec <debug-pod> -- rm /var/lib/byre/hostkey/id_ed25519   # or replace the file
kubectl -n byre-system scale deploy/byre --replicas=1
kubectl -n byre-system logs deploy/byre | grep fingerprint                       # publish the new one
# on every client:
ssh-keygen -R '[<gateway-host>]:<port>'
```

If the build host cannot reach a base-image registry, build the binary first and use the prebuilt
variant, which pulls nothing (`FROM scratch`):

```sh
CGO_ENABLED=0 GOOS=linux go build -trimpath -o bin/byre ./cmd/byre
podman build -f Dockerfile.prebuilt -t <registry>/byre:<tag> .   # docker works the same way
```

Pin the image by **digest**, not by tag: with `imagePullPolicy: IfNotPresent` a re-pushed tag keeps the
node's cached layers, so the Deployment reports the new tag while the running process is still the old
build (`/healthz` shows the truth — it prints the version stamped into the binary).

```sh
podman push --digestfile /tmp/digest <registry>/byre:<tag>
kubectl -n byre-system set image deploy/byre byre=<registry>/byre@<digest>
```

### The grant that is not in the overlay

`kubectl apply -k deploy/` installs the gateway **in its own namespace**. Creating and deleting the
instances it leases happens in a *different* namespace, and kustomize's `namespace:` rewrites every
object at once, so that grant is a separate apply (`deploy/instances-rbac.example.yaml`).

Skipping it produces the most misleading failure this project has: `/healthz` is green, the pool never
warms, and every checkout returns 403 — which reads as "the backend is down". Check it first when the
gateway is alive but cannot hand out instances:

```sh
kubectl -n <instances-ns> auth can-i patch <plural-resource> --as system:serviceaccount:<gateway-ns>:byre
```

`<plural-resource>` is the `resource` from the backend's `provider` block, and the API group is whatever
that block names — the gateway hardcodes neither. The deployment this project was written on uses
`sandboxclaims` from `extensions.agents.x-k8s.io`; that is one claim API among many, not a requirement.

**If `pool_size > 0`,** the gateway will start creating instances immediately. Set `pool_size: 0` in the
ConfigMap for a first smoke test if you do not want warm capacity yet; the value is explicit now (a
literal 0 is no longer rewritten to the default of 2), so `/readyz` is green by construction in that mode.
An explicit `grace: 0s` is still rewritten to the 60 s default; that one is unchanged. And the setting only
applies to drivers that implement `Create` — `endpoints` and `command` never warm anything.

The ConfigMap generated by kustomize carries a content hash in its name (`byre-config-<hash>`). Do not
write `kubectl get cm byre-config` into a runbook — it 404s. Read the name from the Deployment:

```sh
kubectl -n byre-system get deploy byre -o jsonpath='{.spec.template.spec.volumes[?(@.name=="config")].configMap.name}'
```

## Validate a configuration before shipping it

```sh
byre -config /etc/byre/config.yaml -check-config
```

Exits non-zero and lists *every* violation (not the first one) when the file is wrong: unknown fields,
an unknown or missing `provision` value, fields that this provision requires but the file omits,
duplicate fingerprints, malformed endpoints, missing required values.

It checks the configuration, not the cluster: under `-check-config` the Kubernetes drivers are
validated and built without API clients, so this runs on a workstation with no credentials. Whether
the cluster would accept the objects is a separate question — that one is answered by the
`auth can-i` check above and by the first acquire.

It also cannot see intent. `deploy/config.example.yaml` passes with `rc=0` while every value is still a
`<placeholder>` and the fingerprint is 32 zero bytes: the validator checks shapes (base64, `host:port`, a
known `provision`), not whether you meant to edit the file.

It also probes `host_key`: an existing path must be a file, and a missing one must be creatable in its
nearest existing ancestor, because a first start that cannot write the generated key dies halfway. An
empty `host_key` is legal (ADR-0006 keeps it for tests) and is printed as a warning instead of failing.

## Health

Three paths exist on `:8099`:

| endpoint | question | used by |
|---|---|---|
| `GET :8099/healthz` | is the process alive? prints `ok <version>` from the binary | livenessProbe |
| `GET :8099/readyz` | can this process provision instances? | readinessProbe |
| `GET :8099/metrics` | Prometheus text | scraping |

`/readyz` is 200 when there is no pool, or at least one instance is warm, or the last fill created one; it
is 503 otherwise and its body names the pool target, the warm count and the last error (ADR-0011). The
bundled manifest uses `/healthz` for liveness and `/readyz` for readiness — a rollout that cannot
provision stays red instead of going green behind a broken pool. What `/readyz` still does *not* say: the
instances it hands out may be broken in ways only a real session reveals (see
[the grant that is not in the overlay](#the-grant-that-is-not-in-the-overlay) and ADR-0005).

The port is compiled in: `&http.Server{Addr: ":8099"}` in `cmd/byre/main.go:147`, with no config key and
no flag. Two consequences: a second gateway on the same host cannot start — it binds its SSH listener,
logs `health endpoint failed: listen tcp :8099: bind: address already in use` and shuts down — and the
Deployment must never schedule two replicas per node (it is single-replica by design anyway).

The complete metric list:

| metric | type | meaning |
|---|---|---|
| `byre_up` | gauge | always 1 while the process serves |
| `byre_build_info{version,commit,build_date}` | gauge | what is actually running (`/healthz` prints the same stamp) |
| `byre_leases_active` | gauge | leases with at least one open connection |
| `byre_leases_idle` | gauge | leases without connections, waiting for the reaper |
| `byre_leases_adopted` | counter | leases recovered from existing instances at startup |
| `byre_leases_adopted_pending` | gauge | adopted leases no connection has picked up yet |
| `byre_oldest_idle_lease_age_seconds` | gauge | oldest idle lease; growing past `ttl+grace` means the reaper is stuck |
| `byre_pool_target{backend}` | gauge | warm capacity this backend is trying to hold (0 = no pool) |
| `byre_pool_warm{backend}` | gauge | warm instances per backend; 0 for drivers without a pool (`endpoints`, `command`) |
| `byre_pool_inflight{backend}` | gauge | creations in progress |
| `byre_pool_fill_seconds` | histogram | duration of one fill, including the failures inside it |
| `byre_pool_fill_errors_total` | counter | failed instance creations during a fill |
| `byre_handshake_fail_total` | counter | readiness probes where the port answered but SSH did not |
| `byre_release_errors_total` | counter | failed release attempts (one release may count up to three) |
| `byre_acquire_total{backend,result}` | counter | acquisitions by outcome: `reuse`, `pool`, `created`, `error` |
| `byre_process_resident_memory_bytes` | gauge | RSS from `/proc`; the number the memory limit is judged by |

Alerting: use `byre_pool_warm < byre_pool_target` **and only when `byre_pool_target > 0`** — a static
backend has no pool and a naive comparison fires forever.

## Tunables

| key | default | guidance |
|---|---|---|
| `lease.ttl` | `360s` | must exceed the client's idle timeout (reference client: 300s) |
| `lease.grace` | `60s` | reconnect window after a disconnect; absorbs `ControlPersist` expiry |
| `lease.pool_size` | `2` | warm instances; each one costs one pod-worth of memory when idle. `0` is a real value: no warm capacity, every acquire provisions on demand |
| `provider.ready_timeout` | `120s` | raise for slow images (cold layer pulls) |
| `provider.run_as_root` | `false` | workload instances: by default they run as non-root with all capabilities dropped and `RuntimeDefault` seccomp; set true only for an image whose entrypoint genuinely needs uid 0 |
| `ssh.host_key_policy` | `accept-new` | `strict` requires known_hosts; `insecure` is for throwaway instances only |
| `provider.api_group/api_version/resource/object_kind` | — | required for `provision: claims`: the claim API is configuration, never hardcoded |
| `provider.endpoint_field` | `status.sandbox.serviceFQDN` | where a claim-shaped API exposes its SSH address; a bare host gets `:22`. Only a default — set it to your API's path |
| `provider.image` | — | `provision: workloads`: any image that runs sshd and trusts the pool key |
| `command.acquire/release/list` | — | `provision: command`: the program that creates, destroys and (optionally) lists instances |

Warm capacity is reported as `byre_pool_warm{backend=...}` against `byre_pool_target{backend=...}`. A pool
that never reaches its target means `Create` is failing (image pull, quota, missing template) — check the
gateway log for `pool warm failed` and `byre_pool_fill_errors_total`.

## Flags

| flag | default | meaning |
|---|---|---|
| `-config` | `config.yaml` | the configuration file |
| `-listen` | whatever the file says | overrides `listen` for one run: `-listen 127.0.0.1:2223` |
| `-check-config` | off | validate, print every violation, exit 2 on any |
| `-version` | off | print the version stamped into the binary |

```sh
byre -config /etc/byre/config.yaml -listen 127.0.0.1:2223   # second port, same config, no edit
```

## Logs

One line per event, JSON:

```json
{"level":"INFO","msg":"lease acquired","identity":"a1b2c3d4e5f60718","instance":"...","backend":"k8s-claims","elapsed_ms":1203}
{"level":"INFO","msg":"lease released","identity":"a1b2c3d4e5f60718","instance":"...","idle_s":362}
```

Identities appear as the first 16 characters of the fingerprint — enough to correlate, not enough to
reconstruct a key.

## Common symptoms

| symptom | likely cause | action |
|---|---|---|
| client's files vanish between commands | `lease.ttl` shorter than the client's idle timeout, or the client opens connections the gateway treats as new identities | raise `ttl`; check that the client uses one key everywhere |
| many instances, one client | client rotates keys, or authenticates with several keys | alias the fingerprints to the same backend; prefer one key per identity |
| instance never reclaimed | `grace` very large, or a channel leaked (session kept open) | inspect `/metrics` (M3); kill the lease manually via the backend |
| `unable to authenticate` after a key change | new fingerprint not in `identities` | add it (append), reload |
| startup exits with a long error list | configuration violations | fix every listed item; do not "fix one and retry" |

## Restarting or upgrading safely

1. The new process adopts live leases first and destroys unannotated (stale pool) objects second, then
   refills the pool. Expect `byre_leases_adopted` to be non-zero when clients were connected.
2. Compare `byre_leases_active + byre_leases_adopted_pending` with the value of `byre_leases_active`
   before the restart. An adopted lease has no connection yet, so it is idle by definition; comparing
   `active` alone always reads zero and cannot tell a healthy restart from a lost table.
3. **An adopted lease that no client reclaims is destroyed after `ttl + grace`** (420s with the defaults),
   exactly like any other idle lease. That is the documented contract: reconnect inside the window.
4. **Only `claims` and `workloads` survive a restart.** Their instances carry the identity as an
   annotation, so the new process can read it back. An `endpoints` backend keeps ownership in memory
   only: after a restart every endpoint is up for grabs and the next connection is round-robined as if
   nothing had happened. Nothing breaks — but a client that held a tree on one host is now on another.
5. A client that reconnects during this window keeps its instance. A client whose lease had already
   expired gets a new one — that is the documented contract, not a regression.
6. Watch `byre_pool_warm{backend=...}` return to `byre_pool_target{backend=...}`; a pool that stays at 0
   means `Create` is failing — the gateway logs `pool warm failed` every 15s, counts it in
   `byre_pool_fill_errors_total`, and `/readyz` stays red.
7. An instance is bound to an object UID as well as a name. If the object behind a lease was deleted and
   recreated, the gateway logs `stale-binding` and does **not** hand the replacement to the old client; the
   leftover is reclaimed as unbound capacity.
8. **Every SSH connection is cut when the process exits.** There is no draining and no connection
   hand-off: a rollout ends every live session, including a `ssh -L` tunnel. Clients that reconnect
   inside `ttl` + `grace` land on the same instance; long-running commands on the client's side of the
   connection do not survive it. Roll in a window where that is acceptable, or keep the process alive
   and swap configuration only where the binary allows it.

More symptoms, with the log lines that identify each one: [troubleshooting.md](troubleshooting.md).

## Smoke test against a running gateway

`make` and the script are the same thing; the script is what CI would call if it had a gateway.

```sh
BYRE_CFG=config.yaml BYRE_KEY=path/to/client_key test/e2e/run.sh
make e2e CONFIG=config.yaml KEY=path/to/client_key          # identical
# add --ttl-ok (script) to include the 320s idle-reconnect check
```

Without `BYRE_CFG` and `BYRE_KEY` (or `CONFIG`/`KEY`) the script prints `No target given` and exits 2 —
that is the whole content of `make e2e` on its own. A `kind`-based mode is planned in `docs/roadmap.md`;
it needs a container runtime to build and load the image, so nothing calls it yet.

It asserts against a gateway that is already running: same instance across reconnects, `-L` reaching the
instance's own sshd on its port 22, and it prints the adoption and pool counters. Because of that `:22`,
a lab whose endpoints listen on other ports fails the tunnel check — that is the harness, not byre.
It is the same sequence recorded in `docs/acceptance/`.

## Upgrading

1. `byre -check-config` with the new binary against the current file.
2. Roll the Deployment; the manager adopts live instances on startup (ADR-0002 consequences).
3. Watch `byre_leases_active + byre_leases_adopted_pending` return to the previous value of
   `byre_leases_active`; a permanent drop means adoption failed and instances are being recreated under
   clients that still hold state.

## Uninstalling

Delete the Deployment and Service, then delete instances carrying `byre/managed=true`. Static hosts
are untouched by design: the gateway never destroys what it did not create.
