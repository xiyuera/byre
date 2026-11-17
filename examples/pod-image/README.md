# Example: any image that speaks SSH (`provision: workloads`)

This is the escape hatch: byre creates a bare pod from an image you name, waits for it to accept SSH,
and deletes it at the end of the lease. No CRD, no template, no orchestrator-specific wiring.

## A minimal image

```dockerfile
# examples/pod-image/Dockerfile
FROM alpine:3.20
RUN apk add --no-cache openssh-server \
 && ssh-keygen -A \
 && mkdir -p /root/.ssh && chmod 700 /root/.ssh \
 && sed -i 's/^#*PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config
COPY authorized_keys /root/.ssh/authorized_keys   # the gateway's pool public key
EXPOSE 22
CMD ["/usr/sbin/sshd", "-D", "-e"]
```

Build and push it to whatever registry your cluster can pull from. Nothing in this file is
byre-specific: any image that runs `sshd` and trusts the pool key works.

## Backend

This is a **fragment**: it goes under `backends:` in a complete configuration, which also needs `listen`,
`host_key` and an `identities:` list (see [`../local-quickstart/config.yaml`](../local-quickstart/config.yaml)).

```yaml
backends:
  - name: adhoc
    provision: workloads
    namespace: "<namespace>"
    provider:
      image: "<registry>/<sshd-image>:<tag>"
      start_command: ["/usr/sbin/sshd", "-D", "-e"]   # optional; image CMD is used when omitted
      ready_timeout: 180s                            # cold image pulls are slow
    ssh:
      user: root
      private_key: /etc/byre/keys/id_key
      host_key_policy: accept-new
```

## Notes

- The pod is annotated `byre/managed=true` so a restarted gateway can adopt or reap it.
- `ready_timeout` covers image pull, not just boot; a cold multi-hundred-MB image regularly needs >120s.
- No volume is attached: instances are disposable by design. Clients that need state must move it in and
  out themselves (that is exactly what the reference client's file sync does).
- Prefer `claims` when you have a pooled CRD: warm capacity is the difference between a 300 ms and
  a 30 s first connection.
