# Changelog

All notable changes are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

A version is cut when there is something a user would notice. There is no calendar cadence: a release
that contains only "some commits happened" is noise, and a fix that affects users should not wait for a
date. See [docs/release.md](docs/release.md) for the rule and the mechanics.

## [Unreleased]

## [0.3.0] - 2026-11-17

The configuration schema now says what a backend *does*, instead of naming the product it happens to
be (ADR-0007). For an existing installation this is breaking: no 0.2.x configuration file loads
unchanged.

### Changed (breaking)

- `backend.type` is gone, replaced by `backend.provision` with four values: `claims` (a controller
  hands one over), `workloads` (we create the pod), `endpoints` (a fixed list of existing hosts) and
  `command` (a program does it). A file that still says `type` fails with a sentence rather than a
  schema error: `backend.type was replaced by backend.provision (claims|workloads|endpoints|command);
  the value "kubernetes" is not used`.
- `provider.kind` is gone. The provision value decides the behaviour: `sandbox-claim` → `claims`,
  `pod` → `workloads`, `static` → `endpoints`.
- Two claim fields are renamed: `provider.claim_resource` → `provider.resource`, and
  `provider.claim_kind` → `provider.object_kind`.
- The `ssh` block now means the same thing under every provision, and readiness lives in one place
  (`provider.ready_timeout`) instead of being described per kind.

Old → new, whole backend:

```yaml
# 0.2.x
backends:
  - name: k8s-claims
    type: kubernetes
    namespace: "<namespace>"
    provider:
      kind: sandbox-claim
      template: "<template-name>"
      api_group: "<api-group>"
      api_version: "<api-version>"
      claim_resource: "<plural-resource>"
      claim_kind: "<Kind>"
    ssh: { port: 22, user: "<remote-user>", private_key: "/etc/byre/keys/id_key" }

# 0.3.0
backends:
  - name: k8s-claims
    provision: claims
    namespace: "<namespace>"
    provider:
      template: "<template-name>"
      api_group: "<api-group>"
      api_version: "<api-version>"
      resource: "<plural-resource>"
      object_kind: "<Kind>"
    ssh: { port: 22, user: "<remote-user>", private_key: "/etc/byre/keys/id_key" }
```

The upgrade is the rename itself; nothing changed in what the gateway does at runtime. Run
`byre -check-config -config <file>` with the new binary before rolling it: the four provisions are
validated without contacting a cluster.

### Added

- `provision: command`, the escape hatch: an external program provisions the instance and prints one
  JSON object per acquire (`{"id","addr","user?","private_key?"}`). An optional `list` command lets a
  restarted gateway adopt what it left running; without it, orphans from that backend are the
  operator's to clean up, and the configuration says so where you write it.
- `deploy/config.example.yaml` now shows all four provisions, each with only the fields it needs.

### Fixed

- `-check-config` no longer needs cluster credentials. It validates the provisioning configuration and
  builds the drivers without API clients, so the shipped example returns 0 on a workstation that has
  never seen a cluster (it used to exit 2 with "unable to load in-cluster configuration", which made
  the documented pre-flight check unusable outside the cluster).
- The placeholder fingerprint in `deploy/config.example.yaml` was not valid base64, so the example the
  project ships failed its own validation.
- Error text and two tests still said `provider.kind` and `claim_resource`/`claim_kind`.
- Three files arrived unformatted in `e6fa413`; `make lint` (gofmt) is green again.

### Documentation

- `README.md` is rewritten to argue the design instead of announcing it: the connection pattern that
  breaks per-connection allocation, the four repairs that look obvious, the three invariants with the
  commands that check them, what the lease costs, and what is known to be wrong. `README.zh-CN.md`
  follows it section for section.
- `docs/architecture.md`, `docs/operations.md`, `docs/roadmap.md`, `docs/release.md`,
  `docs/integrations/agent-runtime.md`, `examples/` and `deploy/rbac.yaml` use the provision
  vocabulary. The old words survive only where they are history: ADR-0003 (marked as superseded on
  the schema, with a pointer to ADR-0007), ADR-0007 itself, and the acceptance logs.

## [0.2.0] - 2026-11-17

First release that is safe to publish. Two things make it breaking for existing installations, both
documented in `docs/rename-to-byre.md`:

### Changed (breaking)
- The project, module, binary, manifests, namespace and annotation prefix are **byre** (`byre.io/*`).
  An existing `keypod` deployment keeps running untouched; moving it is the cutover in the rename document.
- The gateway's own ServiceAccount now lives beside the cross-namespace Role the manifests always needed.

### Added
- `NOTICE`, `THIRD_PARTY_NOTICES.md`, DCO, `CHANGELOG.md`, `docs/release.md`, `docs/README.md`, release pipeline.
- `docs/adr/0006`: the product owns host key persistence (PVC, generated on first start).


### Added
- `NOTICE` and `THIRD_PARTY_NOTICES.md` with the licence of every Go module and the digest of the licence
  file it was read from (Apache-2.0 §4(d) requires the `yaml.v2`/`yaml.v3` attribution to travel).
- Developer Certificate of Origin: `git commit -s`, a PR checkbox, and a CI check per commit.

### Changed
- The project is renamed **keypod → byre** (module path, manifests, docs). The rename is recorded with its
  collision evidence in `docs/rename-to-byre.md`; the running deployment keeps the old names until the
  cutover in that document is executed.
- Deployment manifests now carry the cross-namespace Role the gateway needs, and `fsGroup` matches the
  image's `USER` — a fresh `apply -k` used to produce a pod that could not read its own credentials.

### Fixed
- The host key is persisted on a PVC and generated on first start (`docs/adr/0006`), so a restart no longer
  changes the gateway's identity.
- `docs/operations.md` no longer claims `pool_size: 0` stops instance creation; it does not (the value is
  clamped to the default).

## [0.1.2] - 2026-11-17

### Fixed
- Host key persistence moved from a hand-made Secret to a PVC the product creates; first start generates and
  persists a key, restarts reuse it.
- Deployment pins the image by digest: a re-pushed tag left the node's cached image running while the
  Deployment reported the new tag.

## [0.1.1] - 2026-11-17

### Fixed
- `host_key` became configurable at all: without it the gateway minted a new key per process, so every
  client's `known_hosts` broke on restart.

## [0.1.0] - 2026-11-17

### Added
- First working gateway: publickey identity, session/exec, `-L`/`-R`, warm pool, declarative claim and pod
  backends, adoption after restart.
