# Backends

A backend is one way of obtaining an instance and one way of reaching it. It has exactly two parts:

```yaml
backends:
  - name: <label used in logs and by identities[].backend>
    provision: claims | workloads | endpoints | command    # how an instance comes into existence
    # …the fields that provision needs (one table per shape, below)…
    ssh: {port: 22, user: "<remote-user>", private_key: /etc/byre/keys/id_key, host_key_policy: accept-new}
```

`provision` is the only required discriminator. `type` is the pre-0.3 spelling: setting it is a
configuration error that names the replacement, rather than an unknown-field error.

## The `ssh` block: shared by all four

| field | required | default | meaning |
|---|---|---|---|
| `ssh.user` | yes | — | the login the gateway uses on the instance; not the client's username |
| `ssh.private_key` | yes | — | the gateway's own key **to** the instance, readable by the gateway process |
| `ssh.port` | no | `22` | port to dial; overridden per instance where the driver reports one with a port |
| `ssh.host_key_policy` | no | `accept-new` | `strict` (needs `known_hosts`), `accept-new` (trust on first use, pinned for the process), `insecure` (no check) |

`private_key` is a plain path and **`~` is not expanded** — `~/keys/id` passes `-check-config` and then
fails on the first dial with `read key ~/keys/id: no such file or directory`. Use an absolute path.

The key named here must be in the instance's `authorized_keys`. byre never installs it: for
`endpoints` that is your job, for `workloads` it must be baked into the image, for `claims` the
controller's template must do it, and for `command` your program must do it before printing the record.

## Readiness: one rule, two implementations

Readiness answers "may I hand this to a client yet", and the answer is always "an address accepted a
TCP connection". Never "a status field exists" — a claim's `status.sandbox.serviceFQDN` is written by
the controller seconds before the Service's DNS record resolves, and on a fake-IP network a DNS lookup
succeeds for names that do not exist (ADR-0005).

| provision | readiness | interval | timeout |
|---|---|---|---|
| `endpoints` | none — the endpoint is used as printed | — | — |
| `workloads` | pod has a `podIP` **and** `PodReady`, phase not `Failed` | 250 ms | `provider.ready_timeout` (120s) |
| `claims` | TCP connect to `:22` on the first address that answers | 250 ms | `provider.ready_timeout` (120s) |
| `command` | none — `addr` is used as printed | — | `command.timeout` applies to the program |

For `claims`, the candidates are tried in this order: the path in `provider.endpoint_field` (default
`status.sandbox.serviceFQDN`), then every entry of `status.sandbox.podIPs`. A bare host is normalised
to `host:22`.

## `endpoints`

```yaml
  - name: lab
    provision: endpoints
    endpoints: ["instance-1.example.com:22", "instance-2.example.com:22"]
    ssh: {port: 22, user: "<remote-user>", private_key: /etc/byre/keys/id_key, host_key_policy: strict}
```

| field | required | notes |
|---|---|---|
| `endpoints` | yes | `host:port` entries; validated as such at startup; duplicates rejected |

Nothing is created, nothing is destroyed. `Acquire` returns the endpoint already held by that identity,
otherwise the next free one in round-robin order. Capacity is `len(endpoints)`; beyond it, `Acquire`
returns `no capacity`, which the client sees as a failed session open.

**No pool.** The driver does not implement `Create`, so `pool_size` has no effect and
`byre_pool_warm{backend="lab"}` stays 0.

**No restart survival.** Ownership lives in the driver's memory. After a restart every endpoint looks
free, so a client that reconnects is round-robined again (measured: two identities on 2200 and 2201, one
restart, the second identity came back on 2200). Use `claims` or `workloads` if a lease must outlive the
gateway process.

**No cleanup between identities.** A released endpoint still holds the previous identity's files, so
giving each identity its own endpoint — or wiping the host in your own tooling — is the only safe
default. `host_key_policy: strict` is worth the `known_hosts` work here precisely because these hosts
are long-lived.

## `workloads`

```yaml
  - name: adhoc
    provision: workloads
    namespace: "<namespace>"
    provider:
      image: "<registry>/<image>:<tag>"
      start_command: ["/usr/sbin/sshd", "-D", "-e"]
      ready_timeout: 180s
    ssh: {port: 22, user: "<remote-user>", private_key: /etc/byre/keys/id_key, host_key_policy: accept-new}
```

| field | required | default | notes |
|---|---|---|---|
| `namespace` | yes | — | where pods are created |
| `kubeconfig` | no | in-cluster | path to a kubeconfig, for running outside a cluster |
| `provider.image` | yes | — | must run `sshd` on port 22 and trust the pool key |
| `provider.start_command` | no | image entrypoint | overrides the entrypoint, e.g. `["/usr/sbin/sshd","-D","-e"]` |
| `provider.ready_timeout` | no | `120s` | 180s is a saner default for cold layer pulls |

The pod is created with `restartPolicy: Never`, one container named `instance`, the `byre` managed-by
label, and no volumes — anything that must persist belongs outside the pod. `Release` deletes the pod
with `gracePeriodSeconds: 0`; a pod already gone is not an error.

On readiness timeout the pod is deleted before the acquire fails, so a broken image does not leave a
trail of Pending pods. RBAC: `pods: get, list, watch, create, delete` **plus `patch`** (the identity
annotation is what makes adoption work).

## `claims`

```yaml
  - name: pooled
    provision: claims
    namespace: "<namespace>"
    provider:
      template: "<template-name>"
      warm_pool: "<warm-pool-name>"
      api_group: "<api-group>"
      api_version: "<api-version>"
      resource: "<plural-resource>"
      object_kind: "<Kind>"
      endpoint_field: ["status", "sandbox", "serviceFQDN"]
      ready_timeout: 120s
    ssh: {port: 22, user: "<remote-user>", private_key: /etc/byre/keys/id_key, host_key_policy: accept-new}
```

| field | required | notes |
|---|---|---|
| `namespace` | yes | where the claim object is created |
| `kubeconfig` | no | empty = in-cluster service account |
| `provider.template` | one of the two | a named template the controller instantiates |
| `provider.warm_pool` | one of the two | a pool to claim from; claim APIs differ, so both selectors exist |
| `provider.api_group`, `api_version`, `resource`, `object_kind` | yes | the GVK, sent verbatim; nothing is built in |
| `provider.endpoint_field` | no | path to the address inside the object, default `status.sandbox.serviceFQDN` |
| `provider.ready_timeout` | no | 120s |

The object is created from the fields above, annotated with the identity, and deleted on release. A
claim that never answers within `ready_timeout` is deleted before the acquire fails.

This is the only shape with a recorded cluster run (`docs/acceptance/`). The deployment it was written
against uses `SandboxClaim` from `extensions.agents.x-k8s.io` with a warm pool — `api_group:
extensions.agents.x-k8s.io`, `resource: sandboxclaims`, `object_kind: SandboxClaim`, and `api_version`
whatever your cluster serves (`kubectl api-resources | grep sandboxclaims`). Those values belong in
your config, not in this binary.

## `command`

```yaml
  - name: cloud
    provision: command
    command:
      acquire: ["/usr/local/bin/provision-ci-vm"]
      release: ["/usr/local/bin/destroy-ci-vm"]
      list: ["/usr/local/bin/list-ci-vms"]
      timeout: 120s
      env: ["CLOUD_PROFILE=ci"]
    ssh: {port: 22, user: "<remote-user>", private_key: /etc/byre/keys/id_key, host_key_policy: accept-new}
```

| field | required | notes |
|---|---|---|
| `command.acquire` | yes | argv, not a shell string — no globbing, no pipes, no `$VAR` |
| `command.release` | no | argv; without it, release is a logged no-op |
| `command.list` | no | argv; without it, a restart cannot adopt this backend's instances |
| `command.timeout` | no | 120s per invocation |
| `command.env` | no | `KEY=VALUE` added to every invocation |

### The contract

`acquire` prints exactly one JSON object on stdout and exits 0. Nothing else on stdout — diagnostics go
to stderr, which byre captures and puts in the error message when the exit code is non-zero.

```json
{"id":"vm-17","addr":"192.0.2.17:22","user":"ci","private_key":"/etc/keys/ci"}
```

| key | required | notes |
|---|---|---|
| `id` | yes | stable, unique within the backend, appears in logs and annotations; never put identity material here |
| `addr` | yes | `host:port` the gateway dials |
| `user` | no | falls back to `ssh.user` |
| `private_key` | no | falls back to `ssh.private_key` |

Environment on each call:

| variable | present for | value |
|---|---|---|
| `BYRE_IDENTITY` | `acquire` | the client fingerprint (base64 body) that is asking |
| `BYRE_INSTANCE_ID` | `release` | the `id` from the acquire that created it |
| `BYRE_INSTANCE_ADDR` | `release` | the `addr` from that acquire |

`list` prints a JSON **array** of the same objects and exits 0; entries that fail validation are skipped
with a warning rather than failing the whole list.

### A worked example (20 lines)

```sh
#!/bin/sh
# provision-ci-vm: create one VM, print the record, exit 0. The provider's own CLI is assumed.
set -eu
set -f                      # no globbing on anything we did not write
name="ci-$(date +%s)-$$"
mytool vm create --name "$name" --profile "${CLOUD_PROFILE:-default}" >/dev/null
ip=$(mytool vm ip "$name")
key=/etc/byre/keys/ci
printf '{"id":"%s","addr":"%s:22","user":"ci","private_key":"%s"}\n' "$name" "$ip" "$key"
```

`release` gets `BYRE_INSTANCE_ID` and `BYRE_INSTANCE_ADDR`; `list` prints every VM the tool considers
managed, in the same shape. If you cannot write a reliable `list`, say so in your own runbook: orphaned
instances are then permanently outside the gateway's accounting.

## Failure behaviour, by shape

| failure | what the client sees | what the gateway does |
|---|---|---|
| endpoint unreachable | `Connection to … closed by remote host` | `cannot reach instance` in the log; the lease is invalidated; the next connection gets a new instance |
| instance refuses a session channel | silent exit 255 | `channel failed type=session … open session on target` |
| claim never ready | connection closes after `ready_timeout` | the object is deleted; acquire fails; the next attempt starts over |
| pod image cannot be pulled | same | the pod is deleted; `pool refill failed` repeats while the pool tries to warm |
| pool exhausted (`endpoints`) | `no capacity` | acquire fails immediately, no queueing |
| `command` exits non-zero | connection closes | the program's stderr is included in the error |
| malformed JSON from `command` | connection closes | `acquire printed N bytes that are not one JSON object` |

## Adding a fifth shape

`internal/backend/backend.go` defines the interface — `Acquire`, `Release`, `List`, `Adopt`, `Name`, and
optionally `Bind` (record the identity on the instance) and `Create` (make capacity before it is asked
for, which is what gives a driver a pool). A new shape needs: a constant in
`internal/config/config.go`, its validation, a driver, an `examples/<shape>/` manifest, and a section in
this file. The client contract does not change: the gateway still authenticates on the public key and
still hands out one instance per identity.
