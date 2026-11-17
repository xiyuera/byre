# ADR-0011 — Gateway readiness is "this process can provision", not "the process is alive"

- Status: accepted
- Date: 2026-11-17
- Deciders: maintainers
- Related: ADR-0005 (instance readiness), ADR-0006 (host key), ADR-0010 (multi-replica gate)

## Context

ADR-0005 defines when an *instance* is ready. Nothing defined when the *gateway* is ready, so the
manifests used `/healthz` for both liveness and readiness — and `/healthz` only reports that the HTTP
handler is running.

Measured on the deployed gateway: `/healthz` returned `200`, `byre_up 1`, `byre_pool_warm 0`, and
`kubectl rollout status` went green, while every acquire failed because `Create` could not provision
(template missing, image pull, quota). The readiness signal said "ready" for exactly the condition that
makes the gateway useless: it can accept SSH and cannot give anyone an instance.

A second, related blind spot: `pool_size: 0` was impossible (0 was rewritten to the default of 2), and the
warmer discarded its error, so a dead pool produced one log line at boot and then silence for as long as
the process lived.

## Decision

The process exposes three endpoints with three different meanings:

| endpoint | question | who uses it |
|---|---|---|
| `:8099/healthz` | is this process alive? | livenessProbe (restart on failure) |
| `:8099/readyz` | can this process provision instances? | readinessProbe (remove from Service on failure) |
| `:8099/metrics` | what has it seen? | Prometheus |

`/readyz` is 200 when, for every configured pool:

- the pool target is 0 (no warm capacity configured: nothing to verify), **or**
- at least one instance is warm, **or**
- the most recent fill created at least one instance.

It returns 503 with the target/warm/last-error map otherwise. It deliberately does not require a *full*
pool: a pool one instance short is a latency problem, while a pool that cannot create anything is an
outage, and readiness must not page for the first.

## Consequences

- A gateway that cannot provision stops receiving new SSH connections instead of accepting them and
  failing every session. Existing sessions are unaffected: readiness does not close the listener, and the
  lease table keeps working — which is the point of separating the two signals.
- This depends on the warmer being loud: a failed fill now logs a `pool warm failed` warning every
  interval and increments `byre_pool_fill_errors_total`, so a red `/readyz` always has a matching log
  line and counter rather than a silent 503.
- `pool_size: 0` is now a real, documented setting (see ADR-0006 rotation), and with it `/readyz` is 200
  by construction: there is no warm capacity to be wrong about.
- A deployment with `pool_size > 0` and a permanently failing backend never becomes Ready. That is the
  intended signal, and it is visible (`kubectl describe pod` shows the probe body) rather than hidden
  behind a green rollout.

## Alternatives rejected

| alternative | why not |
|---|---|
| readiness on `/healthz` (the status quo) | reports "the HTTP handler is running"; that is liveness |
| readiness requires `pool_warm == pool_target` | flips during normal checkout/refill churn, so a rolling update would be blocked by healthy traffic |
| readiness requires one successful acquire | couples readiness to client traffic; an idle but healthy gateway would never be ready |
| a background prober that creates and destroys instances | costs an instance per probe interval and can fight the pool for quota |
| expose readiness on the SSH port | no way for a probe to answer without a client key; the HTTP port exists |
