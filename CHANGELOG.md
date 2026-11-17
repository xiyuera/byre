# Changelog

All notable changes are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

A version is cut when there is something a user would notice. There is no calendar cadence: a release
that contains only "some commits happened" is noise, and a fix that affects users should not wait for a
date. See [docs/release.md](docs/release.md) for the rule and the mechanics.

## [Unreleased]

### Added

- **The contribution rules are in the repository, and `main` enforces them.** `CONTRIBUTING.md`
  now says how a change reaches `main` — branch naming, the four required checks, the
  `Signed-off-by` trailer and why a merge commit or a squash defeats it, who reviews, how a tag
  relates to `main`, and the two operations that cannot go through a pull request. The forge side
  is `enable_push: false` / `enable_force_push: false` with one required approval, which rejects a
  direct push from every account including the owner's. `docs/tag-map.md` records what each tag
  points at after the history rewrite and the two published `commit=` hashes that no longer
  resolve.
- **`make image-push` records the digest it pushed.** It goes through `scripts/publish-image.sh`, which
  writes `dist/images.txt` (`IMAGE_REF` / `IMAGE_TAG` / `IMAGE_DIGEST` / `PUSHED_AT`) and appends
  `IMAGE_DIGEST=` to `dist/build-info.txt`. `make image-verify` (`scripts/assert-image-digest.sh`) exits
  non-zero when the running pod's `imageID` is not that digest — the case a tag cannot catch, because with
  `imagePullPolicy: IfNotPresent` a re-pushed tag keeps the node's cached layers.

### Fixed

- **Release assets go to the release that was just created, not to an older one.**
  `release.yml` derived the release id with a greedy `sed` on the JSON response, which matched the
  *last* `"id"` rather than the release's own: on `POST /releases` that was `author.id` (so the
  v0.3.1 assets landed on the v0.2.0 release), and on a re-run it was the last asset id (so they
  pointed at no release at all). Every upload returned 200, so the job reported success. The pattern
  now anchors on `"id":N,"tag_name"`, which occurs exactly once in a release response, and the
  workflow compares the asset count after uploading instead of trusting per-request status codes.

## [0.3.1] - 2026-11-17

### Added

- Lease and front-end tests that actually run concurrently (`-race` previously had no concurrent code
  path to look at), plus regressions for each item above.
- `byre_instances_discarded` metric; `reason` on the `lease released` log line.
- `docs/adr/0012`: lease-table locking and the `pending`/`leased`/`draining` phases.

### Fixed

- **A slow backend call no longer freezes the gateway.** `Acquire` held the lease table's mutex across
  `drv.Acquire`/`Bind`; a 500ms creation for one identity blocked `Lookup`, `KeepAlive`, `ReleaseConn`,
  `Stats` and every other identity's `Acquire` for the same span (measured: 379ms). Backend calls now run
  with no lock held, a reconnect for an identity whose instance is being created waits on that row instead
  of on the whole table, and the liveness probe on the reuse path runs outside the lock too
  (`docs/adr/0012`).
- **A reaped instance can no longer be handed out while it is being destroyed.** `Reap` deleted the row
  before releasing the instance, so a reconnect in that window received the instance the reaper was
  about to destroy — and once it was released, a second identity could be given the same machine. The row
  is now marked `draining` until the backend has destroyed the instance, and a lease is only removed if
  the table still points at it.
- **A failed dial no longer leaves a dead lease in place.** A deleted instance used to be handed to every
  reconnect for `ttl`+`grace` (420s by default) while healthy capacity sat idle. The front end now asks
  the backend whether the object still exists: a confirmed gap drops the lease (`stale-binding`) and the
  same connection provisions a replacement, while an instance that still exists keeps its lease and is
  counted as unreachable.
- **Pod readiness means "port 22 answers", like the claim path.** `PodReady` is the kubelet's verdict on
  the container's readiness probe; images that start sshd from an entrypoint are Ready before they are
  dialable (ADR-0005).
- **Instances are no longer orphaned on error paths.** An instance that failed `Validate` is released
  (it was leaked), and adoption explicitly destroys instances it cannot own — no identity annotation, a
  duplicate identity annotation, or a description that cannot be dialled — instead of skipping them.
- **`Close()` closes live sessions.** Shutdown used to stop accepting and then wait for clients to hang
  up, so every rolling update reached the grace period and was killed. Sessions are now dropped after
  `-shutdown-delay` (2s by default), and the Deployment sets `terminationGracePeriodSeconds: 20`.
- **`host_key` is required.** An empty value only produced a warning; every restart then changed the
  fingerprint and every strict client stopped connecting. `-allow-ephemeral-hostkey` is the explicit
  opt-out for tests, and it is a flag rather than a config key.
- **An explicit `0` is a value, not a missing key.** `pool_size: 0` (the documented way to run a first
  smoke test without warm pods) silently became 2; `ttl: 0s` and `ssh.port: 0` silently became 360s and
  22. Explicit zeros are now kept where they mean something (`pool_size`, `grace`) and rejected where
  they cannot work (`ttl`, `ssh.port`, `provider.ready_timeout`).
- **A fingerprint must decode to 32 bytes.** A truncated SHA256 fingerprint passed `-check-config` and
  then matched no client.
- **The acceptance script no longer contains always-true assertions.** `check "$A" "$A"` passed when ssh
  failed entirely, and the AC5 line accepted any number. AC5 now reports `N/A` and fails the run unless
  `--allow-na` says the run is not claiming it.
- **`-R 0:host:port` can be requested twice.** The remote-forward listener set was keyed by the requested
  port, so the second request looked like a duplicate and `cancel-tcpip-forward` could never match.
- `known_hosts` appends are verified by reading the line back and serialised with a file lock.

## [0.3.0] - 2026-11-17

The configuration schema now says what a backend *does* rather than naming the product it happens to
be (ADR-0007). **Breaking for existing installations:** no 0.2.x configuration file loads unchanged.

### Changed (breaking)

- `backend.type` is gone, replaced by `backend.provision` with four values: `claims`, `workloads`,
  `endpoints`, `command`. A file that still says `type` fails with a sentence, not a schema error.
- `provider.kind` is gone; `provider.claim_resource` → `provider.resource`,
  `provider.claim_kind` → `provider.object_kind`.
- The `ssh` block means the same thing under every provision; readiness lives in
  `provider.ready_timeout`.

See [CHANGELOG.md](CHANGELOG.md) for the
whole-set migration snippet.

### Also in this tag

The tag was re-cut onto the current `main` before publication, so the assets here are built from
tag `v0.3.0`, which contains the work merged after the first cut:

- **The `command` backend adopts what it left running.** A gateway restart no longer destroys instances
  an agent is still using: the driver lists what it owns, and only what it can prove stale is removed.
- **A lease is invalid the moment its instance is gone** — no more forwarding to an address that no
  longer exists.
- **`/readyz` exists** and a truncated SSH banner is treated as a failure rather than a banner.
- README rewritten: what byre is, a five-minute Quick start, where the SSH backends come from, and the
  design argument moved to `docs/handbook/architecture.md`. Chinese translation follows section by
  section; `docs/handbook/` gains `backends.md` and `troubleshooting.md`.
- Documentation no longer carries this cluster's addresses; the registry section describes the two
  tiers without naming hosts.

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
