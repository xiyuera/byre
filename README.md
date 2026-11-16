# keypod

**A lease-based SSH gateway.** A client presents a public key; it gets one instance for the whole
session, and that instance is held exclusively until the lease expires.

> Status: **M0 (skeleton)** — interfaces, configuration and ADRs are in place; the SSH front end and
> lease manager land in M1/M2. Nothing here is production-ready yet.

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
client ──ssh(pubkey)──> keypod:2222
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
make build     # bin/keypod
make test      # unit tests
make lint      # gofmt + go vet (+ golangci-lint when installed)
make e2e       # end-to-end against a kind cluster
```

## Non-goals for v1

Multi-replica consistency, dynamic multi-backend balancing, in-instance state persistence,
multi-tenancy, a UI, and anything specific to one product (those belong in integration examples).

## License

Apache-2.0 — see [LICENSE](LICENSE).
