# Integrations

Product-specific notes live here and only here. The core (code, config schema, examples) stays generic:
a reader should be able to use keypod without knowing which clients exist.

## What an integration note must contain

1. **What the client does per session** — how many TCP connections it opens, whether it uses
   `ControlMaster`, what its idle timeout is. This is the only thing that decides `lease.ttl`.
2. **How it keeps remote state** — incremental file sync, a working directory, an env snapshot. This
   determines whether the "one instance per identity" rule is a nicety or a hard requirement.
3. **How it authenticates** — which key, and whether the key is stable across restarts (an unstable key
   looks like a new identity on every run).
4. **Configuration** — the exact `backends`/`identities` block, with placeholders.
5. **Verification** — the two-connection test (`hostname` must match) plus one real workload command.

## Rules

- No real hostnames, namespaces, image tags, key paths or identity names. Use `<placeholder>`.
- No client-specific code paths in the core. If a client needs special handling, the integration note
  must first show that configuration alone cannot express it.
- Keep each note short enough to be read before bumping `ttl`: if the reader cannot tell what the
  client's idle timeout is, the note has failed.

## Current integrations

| client | note | verified against |
|---|---|---|
| agent runtime with incremental file sync (spawn-per-call SSH backend, `ControlMaster`, 300 s idle) | [`agent-runtime.md`](agent-runtime.md) | a deployed gateway in a real cluster, non-default identity, `docs/acceptance/2026-11-17-deploy.md` |
