# Example: the Quick start on one machine, no cluster

The README's five minutes, as a file: `config.yaml` in this directory is a complete configuration for
`provision: endpoints` — one host that already exists, which can be the machine you are sitting at.

```sh
ssh-keygen -t ed25519 -N '' -f ~/.ssh/byre_pool     # the gateway's key TO the instance
cat ~/.ssh/byre_pool.pub >> ~/.ssh/authorized_keys  # the instance trusts it (this is your job, not byre's)
ssh-keygen -t ed25519 -N '' -f ~/.ssh/byre_client   # the key your client authenticates with
ssh-keygen -lf ~/.ssh/byre_client.pub -E sha256     # that SHA256: value goes into `identities`
```

Edit two values in `config.yaml` — `<your-user>` and the fingerprint — then:

```sh
go build -o byre ./cmd/byre
./byre -check-config -config examples/local-quickstart/config.yaml   # rc=0, file is self-consistent
./byre -config examples/local-quickstart/config.yaml
ssh -p 2222 -i ~/.ssh/byre_client -o IdentitiesOnly=yes anything@127.0.0.1 'hostname; id -un'
```

Connect twice: both connections name the same instance in the gateway log, because the lease is keyed on
the client key. A key that is not in `identities` is refused during authentication
(`Permission denied (publickey)`), which is the whole access-control model.

What this file does **not** have: a `ssh.port` (the port is in `endpoints` — no driver reads `ssh.port`),
a `pool_size` (an `endpoints` backend creates nothing to pre-warm), and a `namespace` (that is for the
Kubernetes shapes; see `examples/kubernetes-sandbox/` and `examples/pod-image/`).

`-check-config` passing does not mean the file is ready to use: it does not open the key file, reach the
host, or know that `<your-user>` is still a placeholder. The first connection is the real check.

The `endpoints` backend does not survive a restart — ownership is in memory, so a restarted gateway
round-robins again. That is documented in `docs/handbook/backends.md` next to the shapes that do survive.
