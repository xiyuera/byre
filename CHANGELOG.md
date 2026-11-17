# Changelog

All notable changes are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

A version is cut when there is something a user would notice. There is no calendar cadence: a release
that contains only "some commits happened" is noise, and a fix that affects users should not wait for a
date. See [docs/release.md](docs/release.md) for the rule and the mechanics.

## [Unreleased]

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
