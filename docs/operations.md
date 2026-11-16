# Operations

## Deploy

```sh
kubectl apply -k deploy/
```

`deploy/` creates: ServiceAccount, Role/RoleBinding (in the target namespace), Deployment (SSH front end
on 2222, health on 8099), Service (ClusterIP 2222). The configuration comes from a ConfigMap; the pool
key comes from a Secret. Both are examples — copy, then edit.

## Validate a configuration before shipping it

```sh
keypod -config /etc/keypod/config.yaml -check-config
```

Exits non-zero and lists *every* violation (not the first one) when the file is wrong: unknown fields,
unknown backend type or provider kind, duplicate fingerprints, malformed endpoints, missing required
values.

## Health

| endpoint | meaning |
|---|---|
| `GET :8099/healthz` | process alive, returns version stamp |
| `GET :8099/metrics` | Prometheus text (M3: lease/instance counters) |

Suggested probes: liveness `/healthz` with `periodSeconds: 10`, readiness `/healthz` with
`failureThreshold: 3`.

## Tunables

| key | default | guidance |
|---|---|---|
| `lease.ttl` | `360s` | must exceed the client's idle timeout (reference client: 300s) |
| `lease.grace` | `60s` | reconnect window after a disconnect; absorbs `ControlPersist` expiry |
| `lease.pool_size` | `2` | warm instances; each one costs one pod-worth of memory when idle |
| `provider.ready_timeout` | `120s` | raise for slow images (cold layer pulls) |
| `ssh.host_key_policy` | `accept-new` | `strict` requires known_hosts; `insecure` is for throwaway instances only |
| `provider.api_group/api_version/claim_resource` | — | required for `sandbox-claim`: the claim API is configuration, never hardcoded |
| `provider.endpoint_field` | `status.sandbox.serviceFQDN` | where the claim exposes its SSH address; a bare host gets `:22` |
| `provider.image` | — | `pod` kind: any image that runs sshd and trusts the pool key |

Warm capacity is reported as `keypod_pool_warm{backend=...}`. A pool that never reaches `pool_size`
means `Create` is failing (image pull, quota, missing template) — check the gateway log for
`pool refill failed`.

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

## Upgrading

1. `keypod -check-config` with the new binary against the current file.
2. Roll the Deployment; the manager adopts live instances on startup (ADR-0002 consequences).
3. Watch `keypod_leases_active` return to its previous value; a permanent drop means adoption failed and
   instances are being recreated under clients that still hold state.

## Uninstalling

Delete the Deployment and Service, then delete instances carrying `keypod/managed=true`. Static hosts
are untouched by design: the gateway never destroys what it did not create.
