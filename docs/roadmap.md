# Roadmap

Ordered by value, not by effort. Nothing here is promised for a version.

## Done (M0–M4)

Skeleton and ADRs, SSH front end (session/exec/`-L`/`-R`), lease manager with `ttl`/`grace`, warm pool,
`static` and `kubernetes` (pod + declarative claim) drivers, adoption after restart, `/healthz` and
`/metrics`, and a real-cluster acceptance run with a non-default client identity.

## v0.1 — the lease works (M1–M3)

- SSH front end: publickey auth, session/exec, `-L`, `-R`, sftp passthrough.
- Lease manager with `ttl`/`grace`/renewal and the identity table.
- Kubernetes backend: `sandbox-claim` and `pod` kinds; warm pool refill.
- Startup adoption of live instances; `/metrics` with real counters.

## v0.2 — usable by someone else (M4–M5)

Done: `static` backend, an end-to-end run with a real client identity, failure-path tests
(`docs/acceptance/2026-11-17-m5-hardening.md`), and a runbook written from the two defects the cluster
exposed.

Still open: the container image and `kubectl apply -k` path are validated only by server-side dry-run
(no container runtime in the build environment); instance-killed-mid-session and clock-jump injection
remain untested.

## v0.3 — multi-replica

The obvious next bottleneck is that one process holds the truth. Two designs, in order of preference:

1. **Lease objects** (`coordination.k8s.io/Lease`): each identity's lease becomes a Lease object; the
   holder renews it; another replica may steal it after expiry. No new dependency, strong semantics,
   works across replicas in one cluster.
2. **Instance annotations + optimistic concurrency**: write the identity into the instance's annotation
   with `resourceVersion`; whoever loses the race retries with a fresh instance. Survives gateway
   restarts without a separate object, weaker atomicity.

Both need one shared property: *the client must be able to find the replica that holds its lease*.
Options: a headless Service with client-side hashing on the fingerprint, or a consistent-hash ring in
DNS. This is the part that will need the most design.

## v0.4 — more drivers

- `docker` (local sockets, development).
- `remote-kubernetes` (a second cluster as a capacity extension).
- A driver for machines behind a jump host (the gateway dials through, identity preserved).

## Deliberately not planned

- A web UI. The audience is automation; a UI would add an auth surface larger than the product.
- Per-identity quotas and billing. Belongs to a scheduler, not to a transport.
- Persistent home directories per identity. It contradicts "instances are disposable"; clients that need
  it should sync state themselves, as the reference client does.
- Reimplementing SSH in a browser or a custom protocol. Standard SSH is the contract.
