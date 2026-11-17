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
        ┌──────┐  acquire   ┌────────┐  idle > ttl+grace  ┌──────────┐  release   ┌──────┐
        │ idle │───────────>│ leased │───────────────────>│ draining │───────────>│ dead │
        └──────┘            └────────┘                    └──────────┘            └──────┘
           ▲                     │                             │
           │ refill pool         └──── activity: last_seen ─────┘ (back to leased)
           └─────────────────────────────────────────────────────────
```

- `idle` instances are pool members: created ahead of time, never leased to two identities.
- `draining` waits `grace` for a reconnect before the backend destroys the instance; a reconnect during
  `grace` returns it to `leased` (this is what makes `ControlPersist` expiry invisible to the client).
- The table is guarded by a single mutex per identity key; instance accounting is a separate lock so a
  slow backend call never blocks unrelated leases.

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
| `claims` | kubernetes | create an object from `provider.template` and/or `provider.warm_pool` using the configured GVK, then wait until one of the advertised addresses (`endpoint_field`, then `podIPs`) accepts a TCP connection | delete the object |
| `workloads` | kubernetes | create one pod from `provider.image` (+ `start_command`) and wait for readiness | delete the pod |
| `endpoints` | static | pick an endpoint from `endpoints` (round-robin, skip if leased) | mark free |
| `command` | command | run `command.acquire`; the program prints one JSON object describing the instance | run `command.release` |

Those four are the whole vocabulary (ADR-0007). What differs between infrastructures is how an SSH
endpoint comes into existence and how it is destroyed, not how it is reached: `ssh.user`,
`ssh.private_key`, `ssh.port`, `ssh.host_key_policy` and the readiness timeout are shared by all four
and live outside the provision block. No value names a product. A claim-shaped API such as the
`SandboxClaim` object of `extensions.agents.x-k8s.io` is something an operator writes into
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

## 7. Non-goals in v1

Multi-replica coordination, dynamic backend selection, persistent state, per-identity quotas, UI.
The annotation scheme and the identity table are shaped so that coordination can be added later without
changing the client contract.
