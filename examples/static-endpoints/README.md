# Example: static endpoints (no orchestrator at all)

For machines that are always on: lab boxes, build runners, a single beefy host. The gateway does not
create or destroy anything here — it leases the endpoint to one identity at a time and hands it back.

This block is a **fragment**: it goes under `backends:` in a complete configuration, which also needs
`listen`, `host_key` and an `identities:` list (see [`../local-quickstart/config.yaml`](../local-quickstart/config.yaml)).
Run on its own it exits 2 with `at least one identity is required`.

```yaml
backends:
  - name: lab
    provision: endpoints
    endpoints:
      - "192.0.2.11:22"
      - "192.0.2.12:22"
    ssh:
      user: "<remote-user>"
      private_key: /etc/byre/keys/id_key
      host_key_policy: strict     # long-lived hosts should be in known_hosts
```

The port is part of each `endpoints` entry (`host:port`). `ssh.port` parses, but no driver reads it, so
putting `2202` in `ssh.port` and `192.0.2.11` in `endpoints` fails at startup with
`endpoint "192.0.2.11" must be host:port`.

## What the hosts need

1. `sshd` reachable from the gateway pod;
2. the gateway's pool public key in `~/.ssh/authorized_keys` of `user`;
3. nothing else — no agent, no container runtime, no cluster access.

## Behaviour differences

| aspect | `claims` / `workloads` | `endpoints` |
|---|---|---|
| creation | gateway creates the workload | pre-existing |
| release | object deleted | endpoint marked free |
| adoption after restart | by annotation | none — ownership is in memory, so the round-robin starts over |
| blast radius of a bad client | one disposable instance | a shared machine — pin one identity per endpoint |

Because a static endpoint is not disposable, treat it as the exception: give it its own backend name, its
own identity mapping, and `host_key_policy: strict`. Cleanup between identities is the host's problem —
the gateway will not wipe a machine it did not create.
