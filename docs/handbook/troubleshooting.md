# Troubleshooting

Work down this list in order. Each step answers one question and narrows the next: did the client
authenticate, could the gateway reach an instance, did the instance accept a session, did the lease
behave.

Everything the gateway says is one JSON object per line on stdout. Identity appears as the first 16
characters of the fingerprint — enough to correlate, not enough to reconstruct a key.

```sh
journalctl -u byre -f                                  # or: kubectl -n <ns> logs deploy/byre -f
curl -s :8099/metrics | grep '^byre_'                  # the only counters that exist
./byre -check-config -config config.yaml; echo rc=$?   # config only, no cluster
```

## One table of symptoms

| what you see | what it means | first command |
|---|---|---|
| `Permission denied (publickey)` | the client's key is not in `identities` | `ssh-keygen -lf key.pub -E sha256`, compare with the config |
| `Connection to … closed by remote host` (rc 255) | the gateway authenticated the client, then could not reach the instance | grep the log for `cannot reach instance` |
| ssh exits 255 with no message | the instance refused the channel | grep the log for `channel failed` |
| connected, but the files from the last command are gone | the lease expired, or the identity changed | `byre_leases_idle`, and the `instance` field on both `session open` lines |
| `REMOTE HOST IDENTIFICATION HAS CHANGED` | the gateway's host key changed | see [host key](#the-gateways-host-key-changed) |
| connections hang for `ready_timeout` then close | the backend cannot produce a ready instance | grep `pool refill failed` and `no instance for identity` |
| `/healthz` green but every session gets 403 | missing cross-namespace grant | `auth can-i patch … --as system:serviceaccount:…` |
| probe on `/readyz` returns 404 | there is no `/readyz` | point the probe at `/healthz` |

## The client cannot authenticate

```
anything@127.0.0.1: Permission denied (publickey).
```

The gateway offers exactly one authentication method — public key — and only to keys listed under
`identities`. Print the fingerprint the way the gateway computes it, and compare:

```sh
ssh-keygen -lf ~/.ssh/id_ed25519.pub -E sha256
# 256 SHA256:ihSSTfidE8FBRGaLem2sknNk2tDW5LM67WReyPggABU you@laptop (ED25519)
```

`SHA256:` and base64 padding are normalised away, so `SHA256:ihSS…ABU` and `ihSS…ABU` are the same
identity. A different fingerprint is a different identity — there is no wildcard, no per-backend
default, and no anonymous access.

Two clients that present the same key are one identity by design and will contend for one instance.

## The gateway cannot reach the instance

The client sees the connection closed, rc 255. The gateway's line is the one that says why:

```
{"level":"WARN","msg":"cannot reach instance","identity":"ihSSTfidE8FBRGaL","instance":"127.0.0.1:2299","error":"dial 127.0.0.1:2299: dial tcp 127.0.0.1:2299: connect: connection refused"}
```

The three causes seen so far, in the order worth checking:

1. **The endpoint is not listening** — nothing on that `host:port`, or a firewall between the gateway
   and it. `nc -z <host> <port>` from the gateway's own network namespace is the check.
2. **`private_key` starts with `~`** — it is not expanded, and `-check-config` still passes because the
   file is only opened on the first dial:
   ```
   {"level":"WARN","msg":"cannot reach instance",…,"error":"read key ~/keys/id: open ~/keys/id: no such file or directory"}
   ```
   Use an absolute path.
3. **The key is not in the instance's `authorized_keys`** — the dial succeeds and the handshake fails:
   ```
   {"level":"WARN","msg":"cannot reach instance",…,"error":"handshake with 127.0.0.1:2210: ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain"}
   ```
   For `endpoints` you install it; for the other shapes your image, template or provisioner does.

## The instance accepts SSH and refuses the session

The client exits 255 with no message of its own; some clients print `Session open refused by peer`. The
gateway logged the channel, not the connection:

```
{"level":"INFO","msg":"session open","identity":"ihSSTfidE8FBRGaL","backend":"lab","instance":"127.0.0.1:2210"}
{"level":"WARN","msg":"channel failed","type":"session","error":"open session on target: ssh: rejected: connect failed (open failed)"}
{"level":"INFO","msg":"session closed","identity":"ihSSTfidE8FBRGaL","instance":"127.0.0.1:2210"}
```

`session open` means authentication *and* the upstream SSH handshake worked, so the fault is on the
instance: `MaxSessions 0`, a full `MaxStartups`, a forced command that refuses the requested command, or
an `sshd` that has run out of file descriptors. Test the instance directly with the gateway's own key —
if that fails the same way, byre is not involved:

```sh
ssh -i <ssh.private_key> -p <port> <ssh.user>@<instance-addr> true
```

Only `session` and `direct-tcpip` failures are logged this way; `direct-tcpip` failures are what a broken
`ssh -L` looks like.

## A lease is not released

The reclaim clock is `ttl` + `grace`, checked by a reaper that runs every 30 s. Nothing is released
early, and nothing is released on disconnect — that is what the grace window is for.

Measured on a lab gateway with `ttl: 10s` and `grace: 5s`: the client disconnected, the lease was still
counted idle, and `byre_leases_idle` returned to 0 after ~25 s (15 s of clock plus the next reaper tick;
two runs measured 24 s and 25 s).

```sh
curl -s :8099/metrics | grep -E '^byre_leases_(active|idle)'
```

If `byre_leases_idle` stays above zero well past `ttl` + `grace` + 30 s:

- an explicit `grace: 0s` does not mean "no grace": `ApplyDefaults` replaces a zero with 60 s, exactly as
  it does for `pool_size: 0`. The logs print the effective values at startup — read the
  `configuration accepted` line before assuming yours took effect;
- a channel can be held open by a client that is not sending anything; `byre_leases_active` says whether
  something is still attached;
- for `endpoints`, a lease that was never released is invisible after a restart, because ownership lived
  in the process that died.

## The pool does not warm

`byre_pool_warm{backend="…"}` stays at 0 and the log repeats `pool refill failed`.

1. **Only `claims` and `workloads` have a pool.** `endpoints` and `command` cannot create capacity, so
   `pool_size` does nothing for them and the counter stays 0 by design.
2. **The grant is missing.** A gateway that may not create or patch in the instance namespace is alive,
   green on `/healthz`, and refuses every checkout with 403:
   ```sh
   kubectl -n <instances-ns> auth can-i patch <plural-resource> --as system:serviceaccount:<gateway-ns>:byre
   ```
   `patch` matters: the identity annotation is written with a patch, so `create`/`delete` alone is not
   enough.
3. **The image or template is wrong** — `ImagePullBackOff`, a missing template name, a quota. The log
   names the backend and the API error; `kubectl -n <ns> get pods` shows the pod that could not start.

## The gateway's host key changed

```
WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!
```

Every client with `StrictHostKeyChecking` now fails. Two causes, one of which is a configuration mistake:

- **`host_key` is empty.** The gateway generates a key per process and logs
  `no host_key configured: using an ephemeral host key; clients cannot pin this gateway`. Fix the config,
  not the clients.
- **`host_key` points at storage that did not survive** — no PVC, a wrong mount path, a recreated
  container with no volume. The line `generated a new host key and persisted it` at startup is the
  evidence that a *new* key was written; on a restart with durable storage it must not appear.

Deliberate rotation is in [operations.md](operations.md#rotation).

## `-check-config` behaviour

```sh
./byre -check-config -config config.yaml; echo rc=$?
```

- **rc=0** — the file parses, defaults apply, every driver builds. It does *not* mean the cluster will
  accept the objects, that the instance keys exist, or that `endpoints` are reachable.
- **rc=2** — at least one violation, **all of them printed at once**, semicolon-separated inside the
  `error` field. Unknown fields, an unknown or missing `provision`, a `backend.type` left over from
  0.2.x, a duplicate fingerprint, a malformed endpoint (`must be host:port`), a missing `ssh.user` or
  `ssh.private_key`, an identity naming a backend that does not exist.

Fix every listed item. Fixing one and re-running is how a typo survives three deploys.

## `endpoints` hands out a different instance after a restart

Expected. The driver keeps ownership in memory only, so a restarted gateway has no record of which
endpoint an identity held and round-robins from the start again. Measured: two identities on ports 2200
and 2201, one restart, the second identity came back on 2200.

If a lease must outlive the process, use `claims` or `workloads`, where the identity is written onto the
object and read back at startup (`adopted existing leases count=N` in the log).

## Nothing listens on 2222

The front end binds after the configuration is accepted, so a log with `configuration accepted` and no
`ssh front end listening` means the bind failed (`listen …: address already in use`) or the process
exited. `-listen` overrides the configured address, which is the usual way two local gateways end up
fighting over one port during a test.
