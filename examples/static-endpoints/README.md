# Example: static endpoints (no orchestrator at all)

For machines that are always on: lab boxes, build runners, a single beefy host. The gateway does not
create or destroy anything here — it leases the endpoint to one identity at a time and hands it back.

```yaml
backends:
  - name: lab
    type: static
    endpoints:
      - "192.0.2.11:22"
      - "192.0.2.12:22"
    ssh:
      port: 22
      user: "<remote-user>"
      private_key: /etc/keypod/keys/id_key
      host_key_policy: strict     # long-lived hosts should be in known_hosts
```

## What the hosts need

1. `sshd` reachable from the gateway pod;
2. the gateway's pool public key in `~/.ssh/authorized_keys` of `user`;
3. nothing else — no agent, no container runtime, no cluster access.

## Behaviour differences

| aspect | `sandbox-claim` / `pod` | `static` |
|---|---|---|
| creation | gateway creates the workload | pre-existing |
| release | object deleted | endpoint marked free |
| adoption after restart | by annotation | by state file / timeout |
| blast radius of a bad client | one disposable instance | a shared machine — pin one identity per endpoint |

Because a static endpoint is not disposable, treat it as the exception: give it its own backend name, its
own identity mapping, and `host_key_policy: strict`. Cleanup between identities is the host's problem —
the gateway will not wipe a machine it did not create.
