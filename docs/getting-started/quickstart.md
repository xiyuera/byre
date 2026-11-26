---
description: Install byre as one binary or a container, point it at localhost:22, and watch a lease survive a reconnect. The smallest working gateway, in about five minutes.
---

# Quick start

Two ways in. The first needs nothing but Docker; the second needs nothing but a machine you can already
`ssh` to.

## Thirty seconds, with Docker

```sh
git clone https://github.com/xiyuera/byre && cd byre/examples/quickstart
./run.sh
```

The script generates a client key and a pool key, renders a config, builds a sandbox image, starts the
gateway and the sandbox as two containers, and prints the one `ssh` command you need. Then:

```sh
# run the printed command twice
ssh -o StrictHostKeyChecking=no -p 2222 -i ./keys/id_ed25519 sandbox@127.0.0.1 hostname
ssh -o StrictHostKeyChecking=no -p 2222 -i ./keys/id_ed25519 sandbox@127.0.0.1 hostname
```

Both print the same hostname. That is the product. Wait out the 120-second ttl and the instance is
released — connect again and you get a different one, which is the lease ending rather than a bug.

What that directory contains and how to move from it to a real deployment is in
[`examples/quickstart/README.md`](../examples/quickstart.md).

## Five minutes, without Docker

The simplest backend is `provision: endpoints`: nothing is created, the gateway only decides who gets
which host. For this you need one Linux box with `sshd` — it can be the machine you are sitting at.

**1. Build.**

```sh
go build -o byre ./cmd/byre
./byre -version          # dev (none, unknown)
```

**2. Write a config.**

```yaml
listen: ":2222"
host_key: /var/lib/byre/hostkey/id_ed25519

lease:
  ttl: 360s
  grace: 60s

backends:
  - name: lab
    provision: endpoints
    endpoints: ["instance-1.example.com:22", "instance-2.example.com:22"]
    ssh:
      user: "<remote-user>"
      private_key: /etc/byre/keys/id_key
      host_key_policy: strict

identities:
  - fingerprint: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
    backend: lab
```

Replace the user, the hostnames and the key path. Leave everything else alone for now.

The fingerprint is **the client's**, not the gateway's, and `host_key` is the gateway's own key — the one
clients pin. Point `host_key` at durable storage: it is created on first start if absent, and leaving the
field empty gives an ephemeral key that changes on every restart (which breaks pinning).

```sh
ssh-keygen -lf ~/.ssh/id_ed25519.pub -E sha256
# 256 SHA256:ihSSTfidE8FBRGaLem2sknNk2tDW5LM67WReyPggABU you@laptop (ED25519)
```

**3. Validate before starting.** A bad config is worth catching before it is a failed connection:

```sh
./byre -check-config -config ./config.yaml
```

**4. Run, then connect twice.**

```sh
./byre -config ./config.yaml &
ssh -p 2222 -i ~/.ssh/id_ed25519 you@127.0.0.1 hostname
ssh -p 2222 -i ~/.ssh/id_ed25519 you@127.0.0.1 hostname
```

Same hostname twice. With `provision: endpoints` the assignment is stable only while the process lives —
see [Limits](../limits.md).

## Where to go next

| you want | page |
|---|---|
| The four provisioning shapes, and which to pick | [Choose a backend](backends.md) |
| Sandboxes created on demand in a cluster | [Kubernetes](kubernetes.md) |
| How identity, lease, pool and readiness actually fit together | [Architecture](../handbook/architecture.md) |
| It is running and does not work | [Troubleshooting](../handbook/troubleshooting.md) |
