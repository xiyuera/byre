# byre

**English** · [中文](README.zh-CN.md)

**A lease-based SSH gateway.** A client presents a public key; it gets one instance for the whole
session, and that instance is held exclusively until the lease expires.

> Status: **M4** — the lease gateway works end to end: publickey identity, session/exec, `-L`, `-R`,
> warm pool, claim and pod backends, adoption after restart, and real-cluster acceptance with a
> non-default client identity (see `docs/acceptance/`). Still single-replica and without an automated
> e2e gate; hardening is M5.

> Renamed from `keypod` (see `docs/rename-to-byre.md`). Not affiliated with, endorsed by, or derived from
> any same-named project; the name is a cowshed, and the licence grants no trademark rights.

## This is a program, not a library

`module byre` is deliberately a bare path: the project is not meant to be imported, and publishing a
canonical module path would either leak an internal hostname or promise an API that is not kept stable.
`go get` on this module is not supported; fork it or vendor the packages you need.

## Why "per connection" is wrong

Any SSH client opens several TCP connections during *one* session: one for `exec`, one for `scp`/`sftp`,
one for `-L` forwards, plus reconnects after `ControlPersist` expires. If the gateway assigns an instance
per connection, then for any client that keeps remote state across a session (incremental file sync,
working directory, environment snapshot):

| what happens | consequence |
|---|---|
| connection lands on instance B while state was synced to A | the client's "already synced" bookkeeping is wrong; it never re-uploads |
| instance is recycled between two connections | the next command starts from an empty filesystem |
| instance is reused by another identity | state from one user is visible to another |

So the lease is keyed on **identity**, not on a connection:

```
identity := sha256(client public key)
lease(identity) -> instance      # one at a time, exclusive
lease expires after ttl of inactivity, any connection renews it
```

## Architecture (target)

```
client ──ssh(pubkey)──> byre:2222
                          │ 1. auth: publickey -> identity
                          │ 2. lease:  identity  -> lease (memory + instance annotations)
                          │ 3. backend: acquire instance (pool first, then create)
                          └─ 4. open ssh(pool key) to instance, splice channels
                                 ├─ session        (pty / shell / exec)
                                 ├─ direct-tcpip   (-L)
                                 └─ forwarded-tcpip(-R)
```

## Configuration (draft)

```yaml
listen: ":2222"
lease:
  ttl: 360s
  grace: 60s
  pool_size: 2
backends:
  - name: k8s-claims
    type: kubernetes
    namespace: "<namespace>"
    provider:
      kind: sandbox-claim           # sandbox-claim | pod | static
      template: "<template-name>"
      warm_pool: "<warm-pool>"
      image: ""                      # kind: pod
      start_command: []              # kind: pod
      ready_timeout: 120s
    ssh:
      port: 22
      user: "<remote-user>"
      private_key: "<path>"
      host_key_policy: accept-new
  - name: lab-static
    type: static
    endpoints: ["192.0.2.11:22"]
    ssh: { port: 22, user: "<remote-user>", private_key: "<path>" }
identities:
  - fingerprint: "SHA256:<...>"
    backend: k8s-claims
```

Full reference: [`docs/architecture.md`](docs/architecture.md) · decisions:
[`docs/adr/`](docs/adr/) · operations: [`docs/operations.md`](docs/operations.md).

## Build

```sh
make build     # bin/byre
make test      # unit tests
make lint      # gofmt + go vet (+ golangci-lint when installed)
make e2e       # end-to-end against a kind cluster
```

## Dependencies

Five direct modules; the Kubernetes client brings its own transitive set (~40), all from the same
project. Everything the binary links, and why:

| module | role | could it be avoided? |
|---|---|---|
| `golang.org/x/crypto/ssh` | the SSH protocol (server and client) | no — implementing SSH is the one thing this project should not do itself |
| `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery` | talk to the Kubernetes API with client certificates | no, for the `kubernetes` backend; the `static` backend needs none of it at runtime |
| `gopkg.in/yaml.v3` | parse the configuration file | yes, at the cost of JSON-only configuration |

There is **no** gateway/proxy/cache component involved: no sshpiper, no Envoy, no Redis, no sidecar. The
process is a single static binary with two listeners (SSH and health/metrics).

## Non-goals for v1

Multi-replica consistency, dynamic multi-backend balancing, in-instance state persistence,
multi-tenancy, a UI, and anything specific to one product (those belong in integration examples).

## License

Apache-2.0 — see [LICENSE](LICENSE).
