# ADR-0006 — The product owns host key persistence

- Status: accepted
- Date: 2026-11-17
- Deciders: maintainers

## Context

SSH clients identify the gateway by its host key. If that key changes, every client with strict checking
refuses to connect — correctly, because from the client's side "the key changed" and "someone is
impersonating the gateway" are indistinguishable.

The first version generated the key in memory at startup (`host_key` empty). It worked in tests and
failed the first time a deployment was restarted: a pod roll produced a new fingerprint and the profile
that had just been migrated stopped connecting. The key existed, was correct cryptographically, and was
useless because it lived exactly as long as the process.

A first fix made the path configurable and mounted a Secret — but the Secret was created by hand. A fresh
`kubectl apply -k deploy/` therefore either crashed (`host_key` pointing at a missing file) or, if the
field was omitted, silently went back to an ephemeral key. **A packaging step that the operator has to
know about is not a solution to "the install must work".**

## Decision

Persistence is part of the product, in three states:

| `host_key` | behaviour | acceptable for |
|---|---|---|
| empty | ephemeral key, `WARN` at startup | tests only |
| path exists | load it, never rewrite | deployments that supply their own key (Secret, config management) |
| path missing | **generate, write 0600, `WARN` with the fingerprint** | first install |

The bundled manifests give the third case durable storage: a `byre-hostkey` PVC mounted at
`/var/lib/byre/hostkey`. `ReadWriteOnce` is sufficient because v1 runs one replica.

Two boundaries are deliberate:

- `/etc/byre` holds configuration (read-only); `/var/lib/byre` holds state. Mixing them means a
  read-only root filesystem breaks key generation, which is how the first attempt at `fsGroup` failed.
- The key is never derived from anything else (the pool key, the Service account, a hash of config).
  A host key that can be recomputed is not an identity: it can be recomputed by someone else.

## Consequences

- A fresh install produces a working gateway with a stable identity and no manual step; the fingerprint
  appears in the log at first start for the operator to distribute.
- **Losing the PVC means losing the identity.** Clients then must re-pin, which is the same procedure as
  a deliberate rotation. The log line states the new fingerprint for exactly this reason.
- Rotation: replace the file (or delete it and restart), then have clients run
  `ssh-keygen -R '[<host>]:<port>'` and reconnect once. There is no silent re-keying.
- An operator who wants the key in a Secret can still do it: mount the Secret at the configured path.
  The difference is that this is now a *choice* rather than a prerequisite nobody documented.
- Tests cover all three states, including that an existing file is never rewritten
  (`internal/sshfront/hostkey_test.go`).

## Alternatives rejected

| alternative | why not |
|---|---|
| require the operator to create a Secret | a first install that fails until someone reads a doc is not a single-shot deploy |
| generate into an `emptyDir` | same bug with extra steps: the identity still dies with the pod |
| derive the host key from a cluster secret already present | recomputable by anyone with the same access; and it silently changes when that secret rotates |
| disable host key checking in the client | removes the only defence against an impersonated gateway, which is the component that holds the pool key |
