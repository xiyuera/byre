---
description: A plain jump host hands each SSH connection to a free instance; byre binds the whole session to an identity. What that changes, and what it costs.
---

# Why not a plain jump host

A jump host forwards a connection to a target. Handing out an instance per *connection* looks like the same
thing and fails in three places:

| what happens | what the client sees | what actually broke |
|---|---|---|
| connection 2 lands on instance B | every command succeeded | its uploaded tree is on A |
| the lease expires between two commands | `cd: no such file or directory` | the instance was recycled mid-task |
| two agents share a key | one sees the other's files | key distribution, not the gateway |

**The middle case is the expensive one**, because every individual command succeeds. The client's
bookkeeping describes *a* filesystem and the gateway quietly handed it three. There is no error to catch:
the failure surfaces later as a missing file in a build, or as a test that passes locally and not here.

The fix is to key allocation on something that outlives a connection. After authentication SSH carries
exactly one such thing — the public key the client proved possession of. That is the identity, and the lease
hangs off it. No token to pass, no identifier to remember, no `ControlMaster` to keep alive.

## Four repairs that look obvious

Each of these seems to solve it without changing the model, and each fails for a different reason.

**Key on the source address.** An address identifies a network, not a client. Behind NAT every agent on one
laptop shares it, so two agents become one identity; a NAT may also renumber its port mapping after an idle
period, so the address that identified the client at t+0s identifies nobody at t+5min. Deciding "same
client?" from the address means guessing at a value the client's network rewrites.

**Key on the SSH session.** There is no session to key on: a session identifier is created by the connection
that carries it. The gateway terminates SSH, so the only session it can observe is the one it just created —
exactly the object destroyed on disconnect. Reconnects, `scp` and every `-L` forward are new connections with
new sessions, by protocol design.

**Destroy the instance when the connection drops.** This is coherent — the instance's lifetime is the
connection's lifetime — and it leaves the state with nowhere to live between connections, which is the whole
problem. It suits an interactive shell; it does not suit a task that is a sequence of connections.

**Require the client to hold one connection open (`ControlMaster`).** This works until `ControlPersist`
expires, which is exactly the reconnect the design has to survive. It also turns correctness into a client
configuration: any client that does not do it, or that opens a plain socket for `scp` the way the reference
client does, is silently broken.

The full argument, including what keying on the public key costs in exchange, is in
[Architecture](handbook/architecture.md#7-why-the-obvious-alternatives-do-not-work).
