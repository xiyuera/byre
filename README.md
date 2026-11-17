# byre

**English** · [中文](README.zh-CN.md)

An SSH gateway for disposable instances. A client authenticates with a public key; that key decides
which instance it gets, and it keeps that instance for as long as the client is working — not for as
long as one TCP connection happens to live.

> Status: **0.3.0, pre-1.0**. Identity, lease, warm pool, adoption across restarts and all four
> provisioning shapes work end to end, and the numbers in this file come from runs recorded in
> `docs/acceptance/`. Single replica, no automated end-to-end gate, and the image/manifest path is
> verified by server-side dry-run only. See [Limits](#limits-and-things-known-to-be-wrong).

> Renamed from `keypod` (see `docs/rename-to-byre.md`). Not affiliated with, endorsed by, or derived
> from any same-named project; the name is a cowshed, and the licence grants no trademark rights.

## The failure this exists to prevent

An agent picks up a piece of work: sync a working tree to the box, run a build, copy the artefact
back, keep a port forward open while a test suite runs against it. That is one session with a
beginning and an end, and it is four TCP connections:

| # | what it is | when | state it needs to find |
|---|---|---|---|
| 1 | `exec` over the control master | t+0s | nothing; it creates the tree |
| 2 | `scp`/`tar` sync, often a plain socket of its own | t+40s | the tree from #1, minus the files it already sent |
| 3 | `ssh -L` for the test suite | t+90s | the built binary in the working directory |
| 4 | any of the above again after `ControlPersist` expires | t+5min | all of it |

Every one of those connections authenticates from scratch. Nothing on the wire says "I am the same
client as the one three minutes ago" — except the key.

Now allocate an instance per connection. Connection 1 lands on instance A and uploads a working tree
of a few thousand files; the client records `{path: sha}` for each of them and will never send an
unchanged file again. Connection 2 lands on instance B. The build fails, or worse, succeeds on an
empty tree because `mkdir -p` was in the script. Connection 3 forwards a port into a third instance,
where the binary does not exist. The client has no error to report: from its point of view every
command succeeded, and the remote side simply forgot everything between two calls.

That is what makes the bug hard to see. The client is not wrong about anything it observed. Its
bookkeeping is about *a* filesystem, and the gateway quietly gave it four.

The fix has to key allocation on something that outlives a connection. Before authentication an SSH
connection carries almost nothing usable: the version banner is client-chosen text, the username is a
client-side convention that is often empty or shared, and the destination port is the same for
everybody. After authentication it carries one stable, verifiable thing — the public key the client
proved possession of.

## Four repairs that look obvious, and why none of them works

### Sticking to the source address

The cheapest idea, and it fails in both directions. Behind NAT, one address is shared by everything
in that network, so two agents on one laptop become one client. Meanwhile a NAT may renumber its port
mapping after an idle period or a reconnect, so the address and port that identified the client at
t+0s identify nobody at t+5min. A proxy that has to guess "same client?" from the address is guessing
at a value the client's network rewrites.

### Keying on the SSH session

There is no session to key on: a session identifier is created by the connection. The gateway
terminates SSH, so it sees exactly the object that is created and destroyed each time — the thing we
need to look past. Reconnects, `scp`, and every `-L` forward are new connections with new sessions, by
protocol design.

### Destroying the instance when the connection drops

This is what a per-connection container backend does, and it is coherent: the instance's lifetime is
the connection's lifetime. It also means the state has nowhere to live between connections, which is
the whole problem. It suits an interactive shell and it does not suit a task that is a sequence of
connections.

### Requiring the client to hold one connection open (`ControlMaster`)

This works, until `ControlPersist` expires — which is exactly the reconnect the design has to survive.
It also turns correctness into a client configuration: any client that does not do it, or that opens
a plain socket for `scp` the way the reference client does, is silently broken. Pushing a hard
requirement onto callers is how a gateway acquires a support burden it cannot see.

## What it does instead

```
identity := sha256(client public key)
lease(identity) -> instance            # one at a time, exclusive
any connection from that identity renews the lease
instance is reclaimed only after ttl of inactivity, plus grace
```

The lease table is consulted before the pool, so a reconnect cannot race with a recycle. `ttl`
defaults to 360s, which is the reference client's 300s idle timeout plus a margin; `grace` (60s) is
the reconnect window after a disconnect, and it is what makes `ControlPersist` expiry invisible.
Concurrent connections from one identity share the instance — several channels on one target, which
is what SSH is for.

```
client ──ssh(pubkey)──> byre:2222
                          │ 1. auth:    publickey -> identity
                          │ 2. lease:   identity  -> lease (memory + instance annotation)
                          │ 3. acquire: pool first, then create via the backend
                          └─ 4. open ssh(pool key) to the instance, splice channels
                                 ├─ session          (pty / shell / exec)
                                 ├─ direct-tcpip     (-L)
                                 └─ forwarded-tcpip  (-R)
```

## Three invariants, and how to check them yourself

Each of these is one command against a running gateway. They are the contract; if one fails, the
design is not doing what this file says it does.

### The instance does not change inside the lease

Connect, ask the instance for its name, wait 320 seconds (longer than the reference client's 300s idle
timeout, shorter than `ttl` + `grace`), ask again:

```sh
ssh -p 2222 -i <client-key> <user>@<gateway> hostname    # byre-9ac151bc
sleep 320
ssh -p 2222 -i <client-key> <user>@<gateway> hostname    # byre-9ac151bc, or it is broken
```

A different name on the second line means allocation is keyed on the connection, and everything above
is happening to you.

### Different keys never share an instance

Six identities, six connections at once, six distinct names:

```sh
for k in 1 2 3 4 5 6; do ssh -p 2222 -i key$k <user>@<gateway> hostname & done; wait
# observed: 6/6 connected, 6/6 distinct
```

An identity is the key, so this is also the check for "did I accidentally ship one key to two agents":
they are one identity and will share, by design.

### A restart does not lose leases

Start the gateway with a client connected, restart the process without touching the client, then look
for the adoption line in the log and for the same hostname:

```sh
kubectl -n <ns> rollout restart deploy/byre
kubectl -n <ns> logs deploy/byre | grep 'adopted existing leases'   # adopted existing leases count=1
ssh -p 2222 -i <client-key> <user>@<gateway> hostname                # same instance as before
```

If a restart re-created the instance, it would destroy exactly the state the lease was protecting —
which turns "we rolled the gateway" into "every agent lost its working tree".

The record these numbers come from, and the run that produced them, is
`docs/acceptance/2026-11-17-m4.md`. `test/e2e/cluster-acceptance.sh` runs the first and the third
against your own deployment (`--ttl-ok` includes the 320s wait).

## What this costs

A key is an identity, so a shared key is a shared instance. Two agents holding the same private key
are one client: they will contend for one instance, see each other's files, and wait for each other.
That is the design working, and it moves access control into key distribution — aliases in the config
only change log labels, never routing. Rotating a key is a new identity and a cold start.

Idle is measured with the wall clock. An agent that stops to think for longer than `ttl` + `grace`
comes back to a fresh instance and an empty directory, because nothing in the system knows it is
thinking. The knob exists (`lease.ttl`); sizing it above your client's own idle timeout is an
operational duty, not a default that fits everybody.

Instances are not storage. If a file has to survive a lease, it goes somewhere that outlives the
instance, and the client syncs it there.

## Provisioning is a vocabulary

A backend answers one question: how does an instance come into existence, and how do I reach it over
SSH? The answer is one of four values of `provision`, and none of them names a product.

| `provision` | how the instance appears | driver |
|---|---|---|
| `claims` | a controller hands one over when asked; the API coordinates are configuration | kubernetes |
| `workloads` | we create the workload from an image that runs sshd | kubernetes |
| `endpoints` | nothing is provisioned: a fixed set of hosts that already exist | static |
| `command` | an external program provisions and describes the instance | command |

Everything about *reaching* an instance is shared and sits outside the provision block: `ssh.port`,
`ssh.user`, `ssh.private_key`, `ssh.host_key_policy`, readiness timeouts. A backend that speaks SSH is
a backend, whatever created it.

```yaml
listen: ":2222"

lease:
  ttl: 360s            # > your client's idle timeout
  grace: 60s           # reconnect window; absorbs ControlPersist expiry
  pool_size: 2         # warm instances kept ready

backends:
  - name: pooled
    provision: claims
    namespace: "<namespace>"
    provider:
      template: "<template-name>"
      warm_pool: "<warm-pool-name>"
      api_group: "<api-group>"          # the claim API is configuration, never hardcoded
      api_version: "<api-version>"
      resource: "<plural-resource>"
      object_kind: "<Kind>"
      endpoint_field: ["status", "sandbox", "serviceFQDN"]
    ssh:
      port: 22
      user: "<remote-user>"
      private_key: /etc/byre/keys/id_key
      host_key_policy: accept-new

identities:
  - fingerprint: "SHA256:<base64-body>"   # ssh-keygen -lf key.pub -E sha256
    backend: pooled
```

`claims` is an API *shape* — create an object, wait for an address in its status, delete it at the
end — not a particular CRD. The fields `api_group`, `resource` and `object_kind` exist so that one
binary drives any claim-shaped API, including ones nobody here has seen. The deployment this was
written against uses `SandboxClaim` objects from `extensions.agents.x-k8s.io` with a warm pool; that
is one example, and it is configuration in this file rather than a name in the code. A stale
configuration is not a schema error either: `backend.type` is read only in order to say it was
replaced by `backend.provision`.

The other three shapes are shown in `deploy/config.example.yaml` and in `examples/`: an image that
runs sshd, a list of hosts that already exist, and a program that provisions whatever else your
infrastructure has. `command` is the escape hatch with the smallest contract I could write — print one
JSON object (`{"id","addr","user?","private_key?"}`), and optionally implement `list` so a restarted
gateway can adopt what it left running.

Full reference: [`docs/architecture.md`](docs/architecture.md) · decisions:
[`docs/adr/`](docs/adr/) · running it: [`docs/operations.md`](docs/operations.md).

## Limits and things known to be wrong

- **Single replica.** One process holds the lease table, so a second replica would give one identity
  two instances. The designs for multi-replica are written down in `docs/roadmap.md`; neither is
  implemented.
- **`/healthz` does not mean "I can hand out an instance".** It answers "the process is up" and prints
  the version stamped into the binary. A gateway missing its cross-namespace grant is green, never
  warms its pool, and refuses every checkout with a 403 — the most misleading failure this project
  has. `docs/operations.md` leads with it.
- **`provision: command` without `list` cannot adopt.** Instances it created outlive the process and
  nobody reaps them; the config says so when you validate it, rather than at the first restart.
- **No automated end-to-end gate.** The cluster runs in `docs/acceptance/` were executed and recorded
  by hand, and the `kind`-based CI run is still a roadmap item. The image and `kubectl apply -k` path
  are validated by server-side dry-run (`?dryRun=All`) only; the build environment had no container
  runtime to run the image for real.
- **`pool_size: 0` does not stop instance creation.** `ApplyDefaults` treats an explicit zero as an
  absent key. Scale the Deployment to zero if you need to stop it today.

## Prior art, and why it is not used here

These projects are good at what they do; none of them is a lease manager, and reusing one would have
meant writing the missing half anyway.

- **sshpiper** forwards a connection to an upstream chosen from the request — typically the username.
  It never creates an upstream, so there is nothing to lease; and keying on the username reintroduces
  a client-chosen, unverified identifier.
- **ContainerSSH** starts a container for an SSH connection and removes it when the connection ends.
  The lifecycle is the connection's, which is the model this project argues against for agent
  workloads — and it is the right model for a shell.
- **Teleport** authenticates people and machines to servers that already exist, and records what
  happened. It answers "who may reach which host", not "which host does this identity get today".
- **Routers of the kind this replaced** decide at connect time. Whatever they key on — a name in the
  request, a port, a header — the decision is made once per connection, so the next connection is a
  new decision. Making that consistent needs a registry that outlives the connection, which is the
  lease table with fewer features.

## This is a program, not a library

`module byre` is deliberately a bare path: the project is not meant to be imported, and publishing a
canonical module path would either leak an internal hostname or promise an API that is not kept
stable. `go get` on this module is not supported; fork it or vendor the packages you need.

## Build

```sh
make build      # bin/byre
make test       # unit tests
make lint       # gofmt + go vet (+ golangci-lint when installed)
./bin/byre -check-config -config deploy/config.example.yaml
make e2e        # end-to-end against a kind cluster (needs a container runtime)
```

Deploying: `kubectl apply -k deploy/`, **plus** `deploy/instances-rbac.example.yaml`. The grant to
create instances in another namespace cannot live in the overlay, because kustomize's `namespace:`
rewrites every object at once. What skipping it looks like is under Limits, above.

## Where to read next

| document | what it answers |
|---|---|
| [docs/architecture.md](docs/architecture.md) | identity, lease, pool, backends, readiness — how the pieces fit |
| [docs/operations.md](docs/operations.md) | running it: config, first install, rotation, symptoms |
| [docs/roadmap.md](docs/roadmap.md) | what is deliberately absent from v1, in the order it would arrive |
| [docs/release.md](docs/release.md) | when a version is cut, what the pipeline produces |
| [docs/adr/](docs/adr/) | each decision, and the conditions under which it would be reversed |
| [docs/acceptance/](docs/acceptance/) | what was verified on a real cluster, with commands and outputs |
| [docs/integrations/](docs/integrations/) | how a client runtime talks to the gateway |
| [docs/README.md](docs/README.md) | the index of all of it |

## Dependencies

Five direct modules; the Kubernetes client brings its own transitive set (~40), all from the same
project. Everything the binary links, and why:

| module | role | could it be avoided? |
|---|---|---|
| `golang.org/x/crypto/ssh` | the SSH protocol (server and client) | no — implementing SSH is the one thing this project should not do itself |
| `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery` | talk to the Kubernetes API with client certificates | no, for the `claims` and `workloads` provisions; `endpoints` and `command` need none of it at runtime |
| `gopkg.in/yaml.v3` | parse the configuration file | yes, at the cost of JSON-only configuration |

There is **no** gateway/proxy/cache component involved: no sshpiper, no Envoy, no Redis, no sidecar.
The process is a single static binary with two listeners (SSH and health/metrics). Licences, and the
digest of the licence file each module's terms were read from, are in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

## Non-goals for v1

Multi-replica consistency, dynamic multi-backend balancing, in-instance state persistence,
multi-tenancy, a UI, and anything specific to one product (those belong in integration examples).

## License

Apache-2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
