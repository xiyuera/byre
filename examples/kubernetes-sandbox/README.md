# Example: a declarative, pooled sandbox backend

Use this when the cluster already has a CRD that turns a declarative object into a running workload with
a stable endpoint (the reference deployments use one; any equivalent works). The gateway only needs the
two values it cannot invent: **which template to instantiate** and **how long to wait for it**.

## 1. A template (cluster-specific)

```yaml
# examples/kubernetes-sandbox/template.yaml
apiVersion: <your.crd.group>/v1beta1
kind: <SandboxTemplate>
metadata:
  name: sandbox
spec:
  podTemplate:
    spec:
      containers:
        - name: sandbox
          image: "<registry>/<sshd-image>:<tag>"
          ports:
            - containerPort: 22
  service: {}
  # four policies: create/delete/expiry/idle, per your CRD's schema
```

Those angle-bracket placeholders are not valid YAML until you replace them — this is a cluster manifest
to apply with `kubectl`, not a byre configuration file, so `-check-config` has nothing to say about it.

The template must produce a workload that:

- listens on 22 and accepts the gateway's pool key (mounted or injected by your template);
- has a stable, resolvable endpoint for as long as the object exists;
- needs no manual `kubectl port-forward` — the gateway dials it directly from inside the cluster.

## 2. A pool (optional but recommended)

```yaml
# examples/kubernetes-sandbox/warmpool.yaml
apiVersion: <your.crd.group>/v1beta1
kind: <SandboxWarmPool>
metadata:
  name: sandbox
spec:
  replicas: 2      # keep small: each warm instance costs memory while it waits
  templateRef:
    name: sandbox
```

Claims are then created against `warm_pool: sandbox`, which is faster than cold-creating a template.

The claim's status must expose the SSH endpoint at `endpoint_field` (default
`status.sandbox.serviceFQDN`). A bare host is accepted and gets `:22` appended. If your API puts the
address elsewhere, change the path — no code change is needed.

## 3. Point byre at it

The block below is a **fragment**: it goes under `backends:` in a complete configuration, which also needs
`listen`, `host_key` and an `identities:` list (see [`../local-quickstart/config.yaml`](../local-quickstart/config.yaml)).

```yaml
backends:
  - name: claims
    provision: claims
    namespace: "<namespace>"
    provider:
      template: sandbox
      warm_pool: sandbox
      ready_timeout: 120s
      api_group: "<crd-group>"          # e.g. your.sandbox.api
      api_version: "<crd-version>"
      resource: "<plural-resource>"
      object_kind: "<Kind>"
      endpoint_field: ["status", "sandbox", "serviceFQDN"]
    ssh:
      user: "<remote-user>"
      private_key: /etc/byre/keys/id_key
      host_key_policy: accept-new
```

## 4. Verify

```sh
byre -config config.yaml -check-config       # schema and references
kubectl -n <namespace> get <sandboxclaims> -w  # one object appears per identity while leased
ssh -i <client-key> -p 2222 <user>@<gateway>   # auth, then hostname
```

Two connections in a row must print the **same** hostname (ADR-0002). If they differ, the lease is being
keyed on the connection rather than the identity.
