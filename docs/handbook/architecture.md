# Architecture

## 1. Shape

```
             ┌──────────────────────────── byre ────────────────────────────┐
client ──────┤ :2222  ssh front end                                            │
(pubkey)     │          │                                                     │
             │          ├── auth: publickey → identity = sha256(key)           │
             │          ├── lease manager: identity → lease (exclusive)        │
             │          ├── pool: warm instances, refill on checkout           │
             │          └── backend driver: acquire / release / list / adopt   │
             │                                     │                           │
             │ :8099  /healthz  /metrics           │ ssh (pool key)            │
             └─────────────────────────────────────┼───────────────────────────┘
                                                   ▼
                                       instance (any sshd-capable unit)
```

Two listeners, no persistence, no external services. State lives in memory plus instance annotations.

## 2. Connection handling

1. The front end accepts a connection and performs the SSH handshake, offering exactly one auth
   method: `publickey`.
2. On success it computes `identity = sha256(marshaled public key)` and asks the lease manager for a
   lease. The manager either returns the identity's live lease, takes an instance from the pool, or asks
   the backend to create one.
3. The gateway opens its own SSH client connection to the instance using the backend's `ssh` settings
   and the pool key, then splices:
   - `session` (pty-req, shell, exec) ↔ instance session,
   - `direct-tcpip` (`ssh -L`) ↔ instance `direct-tcpip`,
   - `tcpip-forward`/`forwarded-tcpip` (`ssh -R`) ↔ instance remote forwarding.
4. On disconnect the lease stays alive but idle; `last_seen` is refreshed on every frame. Reclaim happens
   after `ttl` + `grace` (ADR-0002).

Splicing is byte-level after channel setup: no protocol inspection of the payload, no re-encoding.

## 3. Lease states

```
        ┌─────────┐  acquire    ┌────────┐  idle > ttl+grace  ┌──────────┐  release   ┌──────┐
        │ pending │────────────>│ leased │───────────────────>│ draining │───────────>│ dead │
        └─────────┘             └────────┘                    └──────────┘            └──────┘
             │                       │                              │
             │ creation failed       └──── activity: last_seen ─────┘
             └── dead                    (refreshed while in use)
```

- `pending` is the row that exists while an instance is being created. A second connection for the same
  identity waits for it and joins the result, rather than starting a second creation.
- `idle` is a state of `leased`: a lease with `refs == 0` that has not yet expired. Pool members are not
  leases; they are warm capacity that becomes one.
- `draining` starts when `ttl` + `grace` have already elapsed, or when a dial failed. The row is kept —
  marked, not deleted — until the backend has destroyed the instance: deleting it first let a reconnect
  through to an instance that was about to disappear. A connection that arrives during the teardown waits
  for it and then gets a new instance.
- The table has one mutex, held only for map and field updates. No backend call (`Acquire`, `Bind`,
  `Release`) is made while it is held, so a slow backend costs the identity that is waiting and nobody
  else. See ADR-0008 for the measured defect, the alternatives and the consequences.

## 4. Recovery

On startup — **before** warming the pool — the manager lists every instance each driver owns
(`app.kubernetes.io/managed-by=byre`) and rebuilds the identity table from the `byre.io/identity`
annotation. Instances without that annotation are the previous process's warm capacity: they are
destroyed and the pool is refilled, so a restart never accumulates orphans.

Two properties make this work and one deliberate limitation applies:

- a pooled instance is bound (`backend.Binder`) the moment it is handed to an identity, so the object
  carries the identity before any client can use it;
- a failed bind returns the instance to the driver instead of leaking it;
- an adopted lease is re-entered idempotently, so the client's next connection gets the same instance —
  and a client that has already died is reaped by the normal `ttl` clock.

## 5. Backends

| `provision` | driver | obtain | release |
|---|---|---|---|
| `claims` | kubernetes | create an object from `provider.template` and/or `provider.warm_pool` using the configured GVK, then wait until one of the advertised addresses (`endpoint_field`, then `podIPs`) answers with a complete SSH banner | delete the object |
| `workloads` | kubernetes | create one pod from `provider.image` (+ `start_command`) and wait for readiness | delete the pod |
| `endpoints` | static | pick an endpoint from `endpoints` (round-robin, skip if leased) | mark free |
| `command` | command | run `command.acquire`; the program prints one JSON object describing the instance | run `command.release` |

Those four are the whole vocabulary (ADR-0007). What differs between infrastructures is how an SSH
endpoint comes into existence and how it is destroyed, not how it is reached: `ssh.user`,
`ssh.private_key`, `ssh.host_key_policy` and the readiness timeout are shared by all four and live outside
the provision block. (`ssh.port` is accepted for compatibility and read by no driver: the port travels in
the instance's `addr`, or in each `endpoints` entry.) No value names a product. A claim-shaped API such as
the `SandboxClaim` object of `extensions.agents.x-k8s.io` is something an operator writes into
`api_group`/`resource`/`object_kind`; it is not a built-in.

The driver interface is deliberately small:

```go
type Backend interface {
    Acquire(ctx context.Context, id Identity) (Instance, error)
    Release(ctx context.Context, in Instance) error
    List(ctx context.Context) ([]Instance, error)
    Adopt(ctx context.Context, in []Instance) error
}
```

`Instance` is `{ID, Addr, User, PrivateKey, HostKeyPolicy, Annotations}` — nothing backend-specific leaks
upward.

A driver that can create capacity ahead of demand also implements `lease.Pooler` (`Create(ctx)`); the
lease manager keeps `pool_size` warm instances for it and refills after each checkout. A driver without
`Create` (the `endpoints` one) simply has no pool — acquire stays on demand, which is the honest model
for machines that already exist.

## 6. Failure behaviour

| failure | behaviour |
|---|---|
| instance dies mid-session | channels close; the lease is invalidated; next connection gets a fresh instance |
| backend API unreachable | acquire fails fast with a typed error; the client sees a clean disconnect and retries |
| pool exhausted | acquire creates an instance on demand (no queueing) |
| gateway restarts | adopt/expire on startup (section 4) |
| config invalid | process exits before binding a port, with all violations listed |

## 7. Why the obvious alternatives do not work

The claim this design makes — allocate on identity, not on the connection — is only worth the code if
the cheaper options fail. Four of them are obvious enough to have been tried by whoever reads this next.

### Sticking to the source address

An address identifies a network, not a client. Behind NAT every agent on one laptop shares it, so two
agents become one identity; a NAT may also renumber its port mapping after an idle period, so the
address that identified the client at t+0s identifies nobody at t+5min. A gateway that guesses "same
client?" from the address is guessing at a value the client's network rewrites.

### Keying on the SSH session

There is no session to key on: a session identifier is created by the connection that carries it. The
gateway terminates SSH, so the only session it can observe is the one it just created — exactly the
object that is destroyed on disconnect. Reconnects, `scp` and every `-L` forward are new connections with
new sessions, by protocol design.

### Destroying the instance when the connection drops

This is what a per-connection container backend does, and it is coherent: the instance's lifetime is the
connection's lifetime. It also means the state has nowhere to live between connections, which is the
whole problem. It suits an interactive shell; it does not suit a task that is a sequence of connections.

### Requiring the client to hold one connection open (`ControlMaster`)

This works until `ControlPersist` expires — which is exactly the reconnect the design has to survive. It
also turns correctness into a client configuration: any client that does not do it, or that opens a plain
socket for `scp` the way the reference client does, is silently broken. Pushing a hard requirement onto
callers is how a gateway acquires a support burden it cannot see.

### The one that is not cheap either

Keying on the public key moves access control into key distribution: a shared key is a shared instance,
and rotating a key is a new identity with a cold start. That is a real cost, paid deliberately, and it is
listed in the README's limits rather than hidden here.

## 8. Non-goals in v1

Multi-replica coordination, dynamic backend selection, persistent state, per-identity quotas, UI.
The annotation scheme and the identity table are shaped so that coordination can be added later without
changing the client contract.
