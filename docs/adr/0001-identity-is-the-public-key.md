# ADR-0001 — Identity is the client's public key

- Status: accepted
- Date: 2026-11-17
- Deciders: maintainers

## Context

The gateway sits at layer 7 (it terminates SSH), so unlike a TCP proxy it can see everything the
protocol exposes: the negotiated algorithms, the authentication method, the public key, the channel
types, the destination of `direct-tcpip` requests.

Before authentication, an SSH connection reveals almost nothing that can be used to route it: the
version banner is attacker-controlled free text, the username is a client-side convention and may be
empty, literal, or shared by many users, and the destination port is the same for everybody. A layer-4
proxy therefore cannot distinguish "tenant A" from "tenant B" at all; the only signals it has are the
source address and the destination port, and mapping tenants onto ports contradicts the requirement
that a tenant must be distinguishable by *name*, not by a number.

## Decision

Identity is `sha256(public key)` of the key the client authenticates with — nothing else.

- The raw key bytes are what the client proves possession of, so the fingerprint cannot be spoofed
  without the private key.
- It is stable across reconnects, across `ControlPersist` expiries, and across client restarts.
- It carries no user-chosen naming, so nothing user-controlled ends up in cluster object names.

The cluster-side object names derive from the fingerprint prefix plus a random suffix; the mapping
`fingerprint → instance` lives in the gateway's memory table and, as a crash-recovery aid, in instance
annotations.

## Consequences

- A client must have a key; password and keyboard-interactive authentication are not identities. They
  are accepted only when a backend explicitly opts in for interactive debugging (not in v1).
- Rotating a key creates a new identity. Operators are expected to map several fingerprints to the
  same backend (aliases only affect logs).
- Two clients sharing one key are one identity and will contend for one instance. That is deliberate:
  the key *is* the account.

## Alternatives rejected

| alternative | why not |
|---|---|
| username | client-chosen, unverified, often empty or shared; nothing prevents impersonation |
| source IP | NAT and multi-host clients make it unstable; also spoofable on a flat L2 |
| destination port | encodes identity into numbers, contradicts name-based identification; burns a port per tenant |
| a proxy-protocol header or env var | unavailable before authentication and only present if the client is configured for it |
