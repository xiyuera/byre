# Integration: an agent runtime with incremental file sync

The reference client for this project. It is an agent that keeps a workspace on the remote side and
talks to it over SSH through a per-session environment object. Nothing here is product-specific except
the numbers, and the numbers are the whole point of the integration.

## 1. What it does per session

| observation | value | why it matters |
|---|---|---|
| connections per session | **2+** | one `exec` master (carries `SendEnv`), plus a plain socket for `scp`/`tar` sync and probes |
| connection reuse | `ControlMaster auto` + `ControlPersist 300` | the master keeps the session's connections alive; when it expires a new TCP connection appears mid-session |
| idle timeout | **300 s** | `terminal.lifetime_seconds`; the environment is torn down after this much inactivity |
| process model | spawn per call (`ssh … bash -c`) | no long-lived shell; every command is a fresh process on the same connection |

Consequence: a lease shorter than ~300 s is guaranteed to be reaped while the client still believes it
holds its instance, and allocation per connection hands the client two different instances inside one
session.

## 2. How it keeps remote state

- It uploads its configuration tree once per session and remembers what it uploaded
  (`{path: sha}`); **it does not re-upload unchanged files**.
- It writes back only files whose hash changed, at teardown.
- It expects its working directory and exported variables to survive between calls on the same host.

If a later connection lands on a different instance, none of that is re-sent: the workspace is simply
missing, and the failure looks like "the agent forgot everything" rather than an infrastructure error.
This is the case ADR-0002 exists for.

## 3. Authentication

- The client authenticates with an **ed25519 key it owns**; the same key is used for every connection in
  the session, and across restarts. That stability is what makes the fingerprint a usable identity
  (ADR-0001).
- A key rotation is a new identity, and therefore a new instance. Expect a one-time cold start.
- The gateway's own key for reaching instances is **different** and never seen by the client.

## 4. Configuration

```yaml
lease:
  ttl: 360s        # > the client's 300s idle timeout, with margin
  grace: 60s       # absorbs ControlPersist expiry and short reconnects
  pool_size: 2

backends:
  - name: claims
    type: kubernetes
    namespace: "<namespace>"
    provider:
      kind: sandbox-claim
      warm_pool: "<warm-pool>"
      api_group: "<crd-group>"
      api_version: "<crd-version>"
      claim_resource: "<plural-resource>"
      claim_kind: "<Kind>"
      endpoint_field: ["status", "sandbox", "serviceFQDN"]
    ssh: { port: 22, user: "<remote-user>", private_key: "<pool-key>", host_key_policy: insecure }

identities:
  - fingerprint: "<sha256 of the client key, no prefix>"
    backend: claims
    aliases: ["<label for logs>"]
```

Client side: point its SSH backend at the gateway's host and port and give it the key whose fingerprint
is listed. Do **not** keep the client's own key in the gateway's backend config — the two keys play
different roles.

## 5. Verification

```sh
# 1. the identity resolves and keeps one instance
ssh -p <port> -i <client-key> <user>@<gateway> hostname   # twice; must match
# 2. a credential file the client syncs is byte-identical on the instance
ssh -p <port> -i <client-key> <user>@<gateway> sha256sum ~/.hermes/credentials/kubeconfig
# 3. a real session through the client's own backend
BYRE_CFG=<config> BYRE_KEY=<client-key> test/e2e/run.sh
```

Expected counters immediately after a session: `byre_leases_active` 1, `byre_leases_idle` 0 (while
connected), `byre_pool_warm` back to `pool_size` after the checkout refills.

## 6. Known client behaviours to design around

| behaviour | handling |
|---|---|
| opens a second connection for `scp`/`tar` | lease is keyed on identity, so both land on the same instance |
| `ControlPersist` expiry mid-session | reconnect within `grace` returns the same instance |
| no re-upload of unchanged files | never move a live identity to another instance |
| teardown after 300 s idle | `ttl` must exceed it; the lease is reaped, the instance is destroyed |
