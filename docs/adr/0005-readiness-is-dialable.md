# ADR-0005 — Readiness means "one advertised address answers", not "a status field is present"

- Status: accepted
- Date: 2026-11-17
- Deciders: maintainers

## Context

A claim-based backend publishes its endpoint in the object's status before that endpoint is usable.
Measured on a real cluster: `status.sandbox.serviceFQDN` appears the moment the controller reconciles
the object, but the Service's DNS record follows a few seconds later. A gateway that treats "field
present" as ready hands its caller an address that fails on the first session with
`dial tcp: lookup <name>: no such host`.

Four of six concurrent sessions failed this way on the first real run.

Worse, the same network made a DNS check useless as a fallback: an upstream resolver answered
**fake-IP** addresses (a synthetic `198.18.x.x` range) for names that do not exist, so `LookupHost`
succeeded for garbage and a TCP connect to the synthetic address could even be accepted by the proxy
that generated it. Any readiness rule phrased in terms of "resolves" is therefore a false positive
generator on such networks.

## Decision

Readiness is a **TCP connect to port 22** on one of the addresses the object advertises, tried in
preference order:

1. the configured `provider.endpoint_field` (default `status.sandbox.serviceFQDN`), normalised to
   `host:port`;
2. every entry of `status.sandbox.podIPs`, same treatment.

The first candidate that completes a TCP handshake wins and is recorded as the instance address. If
none answers within `provider.ready_timeout`, the instance is destroyed and the acquire fails.

This is a *probe*, not a retry policy: once an address has answered, the gateway dials it normally and
lets a failure surface (the lease then drops to idle and is reclaimed).

## Consequences

- The gateway needs network reachability to instances at acquire time — which it needs anyway.
- Readiness costs one TCP handshake per attempt (250 ms interval), negligible against a cold start.
- Fake-IP DNS cannot fool it: a synthetic address either refuses the connection or hangs, and the probe
  simply fails.
- A backend whose status carries no port at all is supported (`host` is normalised to `host:22`).
- The preference order is a heuristic: if `serviceFQDN` points at a Service with a broken selector while
  the pod IP is healthy, the gateway uses the pod IP. That is the behaviour we want, but it means "the
  instance address" is whatever answered first — logged with the lease.

## Alternatives rejected

| alternative | why not |
|---|---|
| trust `status` conditions (`Ready=True`) | the controller sets it with the same delay; it is the field's producer, not an independent observation |
| DNS lookup as the readiness signal | fake-IP resolvers make non-existent names resolve; measured, not hypothetical |
| sleep a fixed 5 s after the object appears | guesses the controller's latency; too short on cold starts, wasteful on warm ones |
| retry the dial in the SSH client instead | moves the wait into every connection, and hides a broken instance behind session timeouts |
