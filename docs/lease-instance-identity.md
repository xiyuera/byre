# Can a client tell that its instance was replaced?

Defect C of the 2026-11-20 report, stated as a design question rather than a bug. Conclusion only: no
code changed for it here.

## The problem

`internal/lease/manager.go` opens with the invariant everything else rests on:

> A reconnect within the lease window must return the same instance, because the client is not going to
> re-sync what it already synced.

With `lease.ttl: 360s` and `grace: 60s`, a session idle for seven minutes is reaped. The next connection
from the same identity is a *new* instance with an empty home directory, while the client believes it has
already pushed everything. Measured consequence: `~/.hermes` missing 1104 files including the kubeconfig,
`shell` exiting 126, a sandbox whose name changes mid-conversation (`sandbox-rckbl → sgkkb → lnp8g`).

Raising `ttl` to `24h` (the ops mitigation) reduces the frequency and changes nothing about the
mechanism: any expiry produces the same silent divergence. **The lease is the thing that expires, so the
client has to be told.** Which is a question about a channel that already exists between byre and the
client, not about a new API.

What byre knows at connect time, and the client does not: the instance's name, its UID (`byre.io/uid`),
and when the object was created. Any of the three answers "same instance or not".

## Options

| # | mechanism | who changes | feasible today | cost |
|---|---|---|---|---|
| A | instance id in the SSH version banner (`SSH-2.0-byre-<id>`) | byre only | yes, ~5 lines (`ssh.ServerConfig.ServerVersion`) | near zero, but **not reliable**: OpenSSH prints the peer banner only at `LogLevel=DEBUG`/on error, and programmatic clients (x/crypto/ssh) see it only if they read `ServerVersion` — which Hermes does not. It is a diagnostic, not a signal. |
| B | `BYRE_INSTANCE_ID` in every session's environment | byre + instance image (`AcceptEnv`) | yes, ~20 lines in `internal/sshfront`: intercept the `env` request and prepend ours before forwarding | low. Reliable for `exec`/`shell` sessions — Hermes runs commands, so it reads it. Requires the target sshd to accept the variable, which is image configuration. |
| C | a file written by byre on bind (`/run/byre/instance`), read by the client | byre + client | yes: byre already holds an SSH key to the instance and can `mkdir`/write one file after `Bind` | low-medium. Adds a per-acquire SSH round trip (~50ms) and a write path; the client must read it, which is the same client change as B. |
| D | keep a reaped instance alive for a retention window so the identity can be re-attached | byre only | yes (`lease.retain_after_expiry`) | high in resources, and it only postpones: eventually the instance must die, and the divergence returns. |
| E | **the client re-syncs unconditionally on every connect**, or verifies a token first | client only | yes, but out of scope here | the real fix; it is the only option that survives an instance that is genuinely gone. |

## Conclusion

- **Not achievable from byre alone, and not worth attempting with A.** A banner is invisible to the
  clients in use; shipping it would look like a fix in a design document and change nothing in a session.
- **B is the cheapest enabling mechanism** and the one to build when the client side is ready: it is
  local to `sshfront`, adds no cluster permissions, and puts the fact exactly where a program will look.
  C is equivalent in effect and worse in moving parts (an extra SSH write per acquire).
- **D is a mitigation, not a fix**, and a more expensive one than the `ttl` change already applied.
- **The fix is E, on the client**: re-push on every new connection, or read the token (B) and re-push when
  it changes. byre's side of that contract is already in place and should not be weakened: a lease keeps
  the instance across reconnects while `refs > 0`, and `byre_acquire_total{result="reuse"}` is the number
  that proves it.

Until E exists, the honest statement to an operator is: *the gateway guarantees instance continuity only
inside the lease window*. `ttl` is the parameter that decides how often that window is tested, and 24h is
a deliberate trade of capacity for continuity, not a repair.

The remaining byre-side gap worth closing separately is smaller and separate: a client that reconnects
after expiry currently has no way to learn that its home is empty except by trying and failing. Making
`ls $HOME` fail loudly at session start (a missing marker file written by the client's own bootstrap)
would turn a 1104-file divergence into one visible error — but that is also client work.
