# Reading an instance's identity from the cluster

Until 2026-11-20 the only place an identity was recorded was one annotation on the claim
(`byre.io/identity`, the client's key fingerprint). Everything else was a constant: the SandboxTemplate
put `hermes.profile: default` on every pod regardless of who held it, so `kubectl get pods` showed a
pool of identical sandboxes and the reasonable-but-wrong conclusion was "the default profile is running
twice".

Two changes make it visible, one in byre and one in the template.

## What byre writes now

On every object it binds (claim or pod):

| key | kind | value |
|---|---|---|
| `byre.io/identity` | annotation | the exactly-normalised fingerprint |
| `byre.io/identity-ref` | label | the same value transliterated for label syntax (`+`→`-`, `/`→`_`, `=` dropped) |
| `byre.io/instance`, `byre.io/uid` | annotations | the object's name and UID |
| anything under `identities[].labels` | labels | operator-declared, for example `hermes.profile: eng` |

Config:

```yaml
identities:
  - fingerprint: "WcHWo47zy7eNKb8CgeTkYX4rjr4CoNcp+xtNo63NYRE"
    backend: sandbox
    aliases: ["eng"]
    labels:
      hermes.profile: eng        # written on every object this identity leases
```

Keys are validated at load: a value Kubernetes would reject fails `-check-config` instead of the first
`Acquire`, hours later. Labels under `byre.io/` and `app.kubernetes.io/managed-by` are reserved.

So the claim is answerable on its own:

```sh
kubectl -n hermes get sandboxclaims \
  -o custom-columns='CLAIM:.metadata.name,IDENT:.metadata.annotations.byre\.io/identity,PROFILE:.metadata.labels.hermes\.profile'
kubectl -n hermes get sandboxclaims -l hermes.profile=eng
```

## Getting it onto the pod

With `provision: claims`, byre creates a claim and the sandbox controller builds the pod from the
SandboxTemplate. byre cannot write the pod's labels: it does not own that object. The pod's labels come
from `spec.podTemplate.metadata` plus whatever the template *propagates* from the claim.

**Check what is installed before choosing:**

```sh
# 1. does the template declare propagation at all?
kubectl -n hermes get sandboxtemplate sandbox -o yaml | grep -n 'propagat' || echo "none declared"
# 2. does the controller honour it? bind an identity, then look at its pod.
kubectl -n hermes get sandboxclaims -l hermes.profile=eng -o jsonpath='{.items[0].status.sandbox.name}{"\n"}'
kubectl -n hermes get pods -l hermes.profile=eng
```

### Variant 1 — the controller propagates labels from the claim

Add the keys to the template's annotations and remove the constant from `podTemplate.metadata.labels`:

```yaml
metadata:
  annotations:
    # copy these label keys from the SandboxClaim/Sandbox onto the pod
    agents.x-k8s.io/propagated-labels: hermes.profile,byre.io/identity-ref
spec:
  podTemplate:
    metadata:
      labels:
        app: sbx-eng
        app.kubernetes.io/part-of: hermes-sandbox
        # hermes.profile: default   <- delete: it is a constant, which reads as an answer
```

If the `kubectl get pods -l hermes.profile=eng` above returns the pod, this is the variant to use and
attribution is one label away from the pod. If it returns nothing, the annotation is being ignored —
this cluster's CRD schema (`extensions.agents.x-k8s.io/v1beta1`) has no field for it, so whether it
works is a property of the installed controller version, not of the configuration.

### Variant 2 — it does not, or the version is unknown

Delete the constant anyway; a label that is identical for every identity is worse than no label. Then
attribute in two hops, which is one query and never lies:

```sh
# claim → sandbox
kubectl -n hermes get sandboxclaim -l hermes.profile=eng \
  -o custom-columns='CLAIM:.metadata.name,SANDBOX:.status.sandbox.name,IDENT:.metadata.annotations.byre\.io/identity'
# sandbox → pod
kubectl -n hermes get pods -l agents.x-k8s.io/sandbox=<sandbox-name>   # or: kubectl get sandbox <name> -o yaml
```

Also worth keeping: distinct pool pods are *expected*. `lease.pool_size` warm instances exist before any
identity claims them, so several sandboxes with no identity is normal; the defect was never the count, it
was that a bound one could not be told from an unbound one.

## What this does not solve

Identity on the object says who holds the instance. It says nothing about whether a client that
reconnects got the *same* instance — see `docs/lease-instance-identity.md` for that question and why it
is client work.
