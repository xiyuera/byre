# Migration record — 2026-11-17: a non-default identity onto the deployed gateway

Charter rule: verify with a non-default identity first, and leave the default identity on its previous
path until the new one has been stable. This record covers moving one profile's SSH backend to the
deployed gateway and what was observed.

## What changed

Exactly three keys in that profile's `terminal` block:

```yaml
ssh_host: "<gateway-service>.<gateway-namespace>.svc.cluster.local"   # was the incumbent proxy
ssh_port: 2222                                                        # was 22
ssh_key: "<profile>/credentials/id_ed25519"                           # was the shared pool key
```

The client now authenticates with **its own** key (the one listed in the gateway's `identities`), while
the gateway reaches instances with its **own** pool key. Before this change every profile presented the
same pool key, which would have collapsed all of them into a single identity — the one failure mode the
identity model cannot tolerate.

Rollback is the same three lines from the backup taken immediately before the change.

## Verification

The check drives the client's real SSH backend (spawn-per-call, incremental sync, `ControlMaster`), not
a hand-rolled `ssh` command:

```
profile ssh: root@<gateway>:2222 key=id_ed25519
instance: keypod-b171c9c2  uid=0
credential sha: local=c843e4f9aa0eef3c remote=c843e4f9aa0eef3c
kubectl whoami: <expected identity>
PASS  session
PASS  credential-synced
PASS  kubectl-usable
PASS  write-read
PASS  stable-instance
```

Notable: the instance is the same one that identity had before the Deployment was rolled and even before
the gateway namespace was deleted and recreated — adoption, not luck (see
`2026-11-17-deploy.md`).

The full acceptance script against the Service also passes:

```
PASS  first connection
PASS  reconnect returns the same instance
PASS  tunnel carries the instance sshd banner      # ssh -L
INFO  leases adopted at startup: 1
INFO  warm instances: 2
passed=3 failed=0
```

## Reaping in the deployed setting

```
t0     instance keypod-b171c9c2   idle 1
t+500  metrics: leases_active 0  leases_idle 0  pool_warm 2
t+500  instance keypod-4896ea62
RESULT: PASS reaped (keypod-b171c9c2 -> keypod-4896ea62)
```

The old instance is gone, the pool did not shrink, and the same identity came back to a fresh instance —
which is the documented outcome after an expired lease, not a regression.

A timing subtlety worth recording: a lease expires after `ttl + grace` (360s + 60s), but the reaper ticks
every 30s. A reconnect scheduled at exactly `ttl + grace` can land *inside the same tick* and still find
the lease alive. An earlier run at `ttl + grace + 10s` reported "same instance" for that reason and
proved nothing; the check needs at least one extra tick (`ttl + grace + 60s` is a safe window).

## Still not migrated

The default identity stays on its previous path, by rule, until this migration has run without incident;
the incumbent proxy still serves the other profiles. Nothing in the gateway requires them to move
together — that is the point of running it as a separate Deployment on its own port.
