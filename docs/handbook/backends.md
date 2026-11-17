# Backends

A backend is one way of obtaining an instance and one way of reaching it. Pick the shape first, then fill
in its fields.

## Which one — decide from what you already have

| what you have | `provision` | why |
|---|---|---|
| hosts that already run `sshd` (lab, build runners, one beefy box) | `endpoints` | nothing is created; the gateway only decides who gets which host |
| a cluster with a claim-shaped CRD and/or a warm pool | `claims` | the controller creates and deletes; byre only asks |
| a cluster and an image that runs `sshd`, no CRD | `workloads` | byre creates one Pod per lease and deletes it |
| cloud VMs, Incus/LXD, a CMDB — anything behind a CLI | `command` | your program provisions and describes the instance |

| `provision` | survives a gateway restart | has a warm pool | needs cluster access | tested on a real cluster |
|---|---|---|---|---|
| `endpoints` | no — ownership is in memory | no (`Create` not implemented) | no | yes |
| `workloads` | yes — identity annotation on the pod | yes | yes (create/delete/patch pods) | unit tests with a fake client |
| `claims` | yes — identity annotation on the object | yes | yes (the configured GVK) | yes (the recorded run) |
| `command` | yes — when `list` reports `annotations.byre.io/identity` | no | no | unit tests |

## The block

```yaml
backends:
  - name: <label used in logs and by identities[].backend>
    provision: claims | workloads | endpoints | command    # how an instance comes into existence
    # …the fields that provision needs (one table per shape, below)…
    ssh: {user: "<remote-user>", private_key: /etc/byre/keys/id_key, host_key_policy: accept-new}
```

`provision` is the only required discriminator. `type` is the pre-0.3 spelling: setting it is a
configuration error that names the replacement, rather than an unknown-field error.

## The `ssh` block: shared by all four

| field | required | default | meaning |
|---|---|---|---|
| `ssh.user` | yes | — | the login the gateway uses on the instance; not the client's username |
| `ssh.private_key` | yes | — | the gateway's own key **to** the instance, readable by the gateway process |
| `ssh.port` | no | `22` | accepted and range-checked, but **no driver reads it**: the port is the one in `addr` or in each `endpoints` entry |
| `ssh.host_key_policy` | no | `accept-new` | `strict` (needs `known_hosts`), `accept-new` (trust on first use, pinned for the process), `insecure` (no check) |

`ssh.port` is the trap: it looks like the way to reach an instance on a non-standard port, and it is not.
A host that listens on 2202 goes into `endpoints` as `"host:2202"`, or comes back from `command.acquire`
as `"addr":"host:2202"`. `endpoints: ["192.0.2.11"]` with `ssh.port: 2202` fails `-check-config` with
`endpoint "192.0.2.11" must be host:port`.

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
    ssh: {user: "<remote-user>", private_key: /etc/byre/keys/id_key, host_key_policy: strict}
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
    ssh: {user: "<remote-user>", private_key: /etc/byre/keys/id_key, host_key_policy: accept-new}
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
    ssh: {user: "<remote-user>", private_key: /etc/byre/keys/id_key, host_key_policy: accept-new}
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
    ssh: {user: "<remote-user>", private_key: /etc/byre/keys/id_key, host_key_policy: accept-new}
```

| field | required | notes |
|---|---|---|
| `command.acquire` | yes | argv, not a shell string — no globbing, no pipes, no `$VAR` |
| `command.release` | no | argv; without it, release is a logged no-op |
| `command.list` | no | argv; a restart adopts the entries that carry `annotations.byre.io/identity` and keeps the rest (see below) |
| `command.timeout` | no | 120s per invocation; bounds the program, not its children |
| `command.env` | no | `KEY=VALUE` **appended** to the inherited environment, never replacing it |

### The contract

`acquire` prints exactly one JSON object on stdout and exits 0. Nothing else on stdout — diagnostics go
to stderr, which byre captures and puts in the error message when the exit code is non-zero. Extra output
on stdout is fatal: `acquire printed 58 bytes that are not one JSON object: invalid character 'p' …`
(`internal/backend/command.go:121-124`).

```json
{"id":"vm-17","addr":"192.0.2.17:22","user":"ci","private_key":"/etc/keys/ci"}
```

| key | required | notes |
|---|---|---|
| `id` | yes | stable, unique within the backend, appears in logs and annotations; never put identity material here |
| `addr` | yes | `host:port` the gateway dials |
| `user` | no | falls back to `ssh.user` (`command.go:104`) |
| `private_key` | no | falls back to `ssh.private_key` (`command.go:105`) |
| `annotations` | no | durable hints, the identity binding among them; must appear in `acquire` **and** `list` for adoption to match (`command.go:106-126`) |

`id` and `addr` are checked after decoding: an empty `id` or `addr` fails the acquire with
`returned an unusable instance: Addr is empty` (`internal/backend/backend.go:35-45`). The `ssh.user` /
`ssh.private_key` fallback still has to be configured, even when every record carries its own:
`NewCommand` refuses a backend without them (`command.go:65-67`), because a record that omits them has
nowhere to fall back to.

Environment on each call:

| variable | present for | value |
|---|---|---|
| `BYRE_IDENTITY` | `acquire` | the client fingerprint (base64 body, no `SHA256:` prefix, no padding) that is asking |
| `BYRE_INSTANCE_ID` | `release` | the `id` from the acquire that created it |
| `BYRE_INSTANCE_ADDR` | `release` | the `addr` from that acquire |

`list` gets **no** `BYRE_*` variables at all (`command.go:144`), and prints a JSON **array** of the same
objects; entries that fail validation are skipped with a warning rather than failing the whole list
(`command.go:148-161`). Measured, with a lister that dumps its environment to a file: the file is empty.

**Failure** is a non-zero exit plus one line on stderr; that line ends up in the gateway's error
(`run "…/acquire.sh": exit status 1 (stderr: quota exceeded for project ci)`, `command.go:87-89`). The
client itself only ever sees `Connection to <host> closed by remote host`.

**`timeout` bounds the program, not its process tree.** The deadline is enforced with
`exec.CommandContext`, which kills the direct child (`command.go:79-81`). A script that leaves a
background process holding stdout keeps the wait alive past the timeout — measured with `timeout: 2s`:
a script ending in `sleep 4 & sleep 4` was waited on for 4s, while the same script with `exec sleep 6`
was cut at 2s. If your provisioner spawns helpers, make it `exec` its real work or `wait` on its own
children.

**`list` is how a restart adopts, and what it cannot attribute it keeps.** `Manager.Adopt` runs before
the pool warms: an instance whose record carries `byre.io/identity` is put back in the lease table
(`internal/lease/manager.go:225-302`). Whether an *unannotated* instance is released depends on the
driver, and only a driver that makes the promise gets that: `kubernetes` and `endpoints` implement
`backend.StaleCapacity` and report every instance they lease as carrying an identity, so an anonymous one
is their own stale capacity (`backend.go:87-99`); `command` does not implement it, so an unannotated
instance is **kept** and logged:

```
{"msg":"instance kept at startup","instance_id":"vm-anon","backend":"cloud","reason":"no byre.io/identity annotation and the driver cannot promise that anonymous instances are stale; destroying it could kill a live lease"}
```

That is ADR-0009, and it is a deliberate leak: destroying something in use is unrecoverable, leaving warm
capacity behind is not. `adoption finished` is printed even when nothing was adopted. To make adoption
work, print `"annotations":{"byre.io/identity":"<fingerprint>"}` in **both** `acquire` and `list`
(`command.go:162-163`) — the fingerprint is the same base64 body `BYRE_IDENTITY` carries.

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

`release` gets `BYRE_INSTANCE_ID` and `BYRE_INSTANCE_ADDR`. Write a `list` if a restart should adopt:
print `"annotations":{"byre.io/identity":"<fingerprint>"}` in both `acquire` and `list`, and the identity
that reconnects gets its instance back. Without a `list`, instances your program creates outlive the
gateway and nothing in byre notices them again — your runbook owns the cleanup.

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
