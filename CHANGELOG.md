# Changelog

All notable changes are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

A version is cut when there is something a user would notice. There is no calendar cadence: a release
that contains only "some commits happened" is noise, and a fix that affects users should not wait for a
date. See [docs/release.md](docs/release.md) for the rule and the mechanics.

## [Unreleased]

### Added

- **`scripts/release.sh` cuts a release in one verified pass.** The first v0.4.0 attempt lost
  half an hour waiting for a GitHub run that could never start: the tag existed on Gitea and not
  on the mirror. Gitea's push mirror propagates a tag *deletion* but not a *re-creation*, and it
  reports no error because nothing failed — there was simply nothing to send. The script now
  pushes the tag to both forges, reads both tag objects back and compares them to the local one,
  waits on each side with a bound, and finishes by checking release assets on both forges and that
  the image manifest answers `200` to an anonymous pull. Tag creation is idempotent: the same
  commit reuses the tag, a different commit is refused instead of silently moved.
- **The identity is written as a label next to the annotation, and operators can add their own.**
  `byre.io/identity-ref` carries the fingerprint in a form a label can hold (the annotation keeps it
  exact), and `identities[].labels` is a new config block whose keys are written on every object that
  identity leases — `hermes.profile: eng` and the like. Without it a cluster could not say who held an
  instance: the SandboxTemplate put the same constant on every pod, which reads as an answer and is not
  one. See `docs/identity-on-the-pod.md`.
- **The reaper sweeps instances that belong to nobody.** Adoption only runs at startup; an object
  orphaned while the process runs (a create that lost its annotate step, a release that failed three
  times) used to survive until the next restart. The sweep runs every 5 minutes, only for drivers that
  promise anonymous instances are stale, only for objects older than 15 minutes, and never for one a
  lease row or the warm pool still references. `byre_instances_reclaimed` counts them separately from
  the startup decisions, because a non-zero value after a long uptime means the create/bind/release path
  is leaking, not that leftovers were tidied.
- **A release publishes the image digest its tag resolved to.** `images.txt` is written by the pipeline
  now, not by hand: `scripts/record-release-image.sh` asks the registry what `<tag>` resolves to, hashes
  the manifest it gets back, and the release uploads that file beside the binaries. A tag whose image was
  never pushed fails the release instead of publishing no digest — v0.3.1 and v0.3.2 were repaired by hand
  for exactly that — so the order is: push the image, then push the tag. The file is deliberately not
  covered by `SHA256SUMS`, which describes what a rebuild reproduces; `PUSHED_AT` is `unknown` because a
  registry manifest carries no push timestamp.

### Changed

- **`make image-verify` asserts the registry as well as the digest.** When the same blobs already exist
  under another registry name, containerd pulls nothing and `imageID` keeps reporting the old repository,
  so a digest-only comparison passes while the pod runs the other image. It exits 2 when it cannot
  qualify the check (a bare digest with no registry to compare against), because "could not check" must
  not read as "checked and fine". `make image-verify IMAGES=images.txt` takes the release's own asset.

### Fixed

- **`list` no longer drops an object it cannot address.** The claim path read only
  `status.sandbox.serviceFQDN` and skipped anything that did not have one, so an object that was ready by
  its pod IP was invisible to `Adopt` and to `Reap`: neither reused nor destroyed, just Running. Two
  claims in that state survived a restart on a live gateway while the log reported two *other* anonymous
  claims discarded. Every selected object is returned now, with the address resolved the way readiness
  resolves it, and one with no address at all is logged rather than hidden. ADR-0014.
- **An image whose binary reports the unstamped default can no longer be published.**
  `scripts/publish-image.sh` runs the image and refuses (`exit 2`) when it prints `dev (none, unknown)`,
  which is what a build without `--build-arg VERSION/COMMIT/BUILD_DATE` produces while every pipeline
  step succeeds. That is how v0.4.0 shipped `byre_build_info{version="dev",commit="none"}`. `make image`
  already supplies the arguments; nothing was checking the artifact.

## [0.4.0] — the sandbox backend ships with the gateway

A reader could previously build the gateway but had to invent the other half. Now both halves
are in the repository, and there is a chart for the Kubernetes shape.

### Added

- `examples/docker-sandbox/` — the complete deployment with no cluster: `command` provisioner,
  sandboxes are containers, `scripts/{keys,acquire,release,list}.sh` implement the three-program
  contract. Verified end to end on aarch64 docker: the same key lands on the same container
  while the lease holds, `byre.io/identity` is recorded so a restart can adopt it.
- `examples/kubernetes-sandbox/` — rewritten from a page of placeholders into applyable files:
  `SandboxTemplate`, `SandboxWarmPool`, the Role that grants only `sandboxclaims`, the gateway
  Deployment, its ServiceAccount, and `verify.sh`. Uses the real
  `extensions.agents.x-k8s.io/v1beta1` API.
- `charts/byre/` — Helm chart. Defaults install and start; `sandbox.enabled` renders the
  template and pool when you have an image to point them at. Every value is overrideable and
  documented inline.

### Changed

- Workflows live in `.github/workflows` only. Gitea's `WORKFLOW_DIRS` falls back to that
  directory when `.gitea/workflows` is absent, so one file serves both forges — which also
  keeps `.gitea/` out of anything mirrored to GitHub.
- The CI checkout is a plain `git clone` against `$GITHUB_SERVER_URL`, and the job token is
  `secrets.GITEA_TOKEN || github.token`, so no job has to fetch an action from the internet.

### Fixed

- The old kubernetes example named `templateRef`; the controller reads
  **`sandboxTemplateRef`**. The wrong field is accepted by the API server and silently
  ignored, leaving a pool with zero warm pods and no error.
- `release.sh` in the docker example reads `$BYRE_INSTANCE_ID`. It is an environment variable,
  not a positional argument (`internal/backend/command.go`).

## [0.3.3] - 2026-11-18

The release pipeline now records the image digest as an asset, so a reader can tell which
binary and which image belong to the same tag without trusting a registry UI.

### Added

- `release.yml` writes `images.txt` (image reference + digest) and attaches it to the release;
  the platform asserts the digest the pipeline recorded matches what the registry reports.
- `scripts/assert-image-digest.sh` fails when the digest is missing, when the registry prefix
  does not match the expected one, and when no credential is configured — three different
  states, three different exit codes.

### Certified

- `v0.3.3` image: `sha256:4af3606f7b5c5e31aa5ac9f8f49f42ee315483d709f536cd28c00d3260c2f86f`,
  matching the running pod in the reference deployment and the binary built from the tag.

## [0.3.2] - 2026-11-18

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

### Added

- Lease and front-end tests that actually run concurrently (`-race` previously had no concurrent code
  path to look at), plus regressions for each item above.
- `byre_instances_discarded` metric; `reason` on the `lease released` log line.
- `docs/adr/0012`: lease-table locking and the `pending`/`leased`/`draining` phases.

### Changed

- `docs/architecture.md` and `docs/operations.md` moved to `docs/handbook/`, and the documentation index
  now separates the handbook from decisions and records. The runbook's log examples were rewritten to the
  lines the binary actually emits.

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
- `provider.kind` is gone, and so is `backend.type`. 0.2.x needed both: `type: kubernetes` +
  `provider.kind: sandbox-claim` → `provision: claims`; `type: kubernetes` + `provider.kind: pod` →
  `provision: workloads`; `type: static` (no `provider` block) → `provision: endpoints`.
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

The `static` backend had no `provider` block at all, so its migration is a separate edit:

```yaml
# 0.2.x
backends:
  - name: lab
    type: static
    endpoints: ["192.0.2.11:22", "192.0.2.12:22"]
    ssh: { port: 22, user: "<remote-user>", private_key: "/etc/byre/keys/id_key" }

# 0.3.0
backends:
  - name: lab
    provision: endpoints
    endpoints: ["192.0.2.11:22", "192.0.2.12:22"]
    ssh: { port: 22, user: "<remote-user>", private_key: "/etc/byre/keys/id_key" }
```

The upgrade is the rename itself; nothing changed in what the gateway does at runtime. Run
`byre -check-config -config <file>` with the new binary before rolling it: the four provisions are
validated without contacting a cluster.

### Added

- `provision: command`, the escape hatch: an external program provisions the instance and prints one
  JSON object per acquire (`{"id","addr","user?","private_key?"}`). An optional `list` command lets a
  restarted gateway adopt what it left running; without it, orphans from that backend are the
  operator's to clean up, and `deploy/config.example.yaml` says so in a comment.
- `deploy/config.example.yaml` now shows all four provisions, each with only the fields it needs.
- `docs/handbook/backends.md`: field tables for the four provisions, the shared `ssh` block, the
  readiness rule per shape, failure behaviour, and the `command` contract with a worked script.
- `docs/handbook/troubleshooting.md`: each symptom with the log line that identifies it, the measured
  reclaim clock, and what `-check-config` exit 2 does and does not mean.
- Two facts that were previously discovered the hard way, now documented and measured: `endpoints` does
  not survive a gateway restart (its ownership is in memory, so the round-robin starts over), and `~` in
  `ssh.private_key` is not expanded — `-check-config` passes and the first dial fails.

### Changed

- The README now opens with what byre is, a five-minute Quick start that runs on one machine, and a table
  of where the SSH backends come from — the design argument that used to fill the file moved into
  `docs/handbook/architecture.md` §7. No code changed.
- `docs/architecture.md` and `docs/operations.md` moved to `docs/handbook/`, and the documentation index
  now separates the handbook from decisions and records.
- The maintenance rules in `CONTRIBUTING.md` point at the handbook, and so does the comment in
  `deploy/instances-rbac.example.yaml`.

### Fixed

- `-check-config` no longer needs cluster credentials. It validates the provisioning configuration and
  builds the drivers without API clients, so the shipped example returns 0 on a workstation that has
  never seen a cluster (it used to exit 2 with "unable to load in-cluster configuration", which made
  the documented pre-flight check unusable outside the cluster).
- The placeholder fingerprint in `deploy/config.example.yaml` was not valid base64, so the example the
  project ships failed its own validation.
- Error text and two tests still said `provider.kind` and `claim_resource`/`claim_kind`.
- Three files arrived unformatted; `make lint` (gofmt) is green again. (The commit that introduced
  them no longer exists after the history rewrite.)
- **The `command` backend adopts what it left running.** A gateway restart no longer
  destroys instances an agent is still using: the driver lists what it owns, and only what it
  can prove stale is removed.
- **A lease is invalid the moment its instance is gone** — no more forwarding to an address
  that no longer exists.
- **`/readyz` exists** and a truncated SSH banner is treated as a failure rather than a banner.

### Documentation

- `README.md` is rewritten to argue the design instead of announcing it: the connection pattern that
  breaks per-connection allocation, the four repairs that look obvious, the three invariants with the
  commands that check them, what the lease costs, and what is known to be wrong. `README.zh-CN.md`
  follows it section for section.
- `docs/architecture.md`, `docs/operations.md`, `docs/roadmap.md`, `docs/release.md`,
  `docs/integrations/agent-runtime.md`, `examples/`, `deploy/rbac.yaml`, `.gitea/ISSUE_TEMPLATE/`
  and `CONTRIBUTING.md` use the provision vocabulary. The old words now appear only where they are
  deliberately kept: this file's migration notes, ADR-0003 (marked as superseded on the schema, with a
  pointer to ADR-0007), ADR-0007 itself, and the acceptance logs.
- README rewritten: what byre is, a five-minute Quick start, where the SSH backends come
  from, and the design argument moved to `docs/handbook/architecture.md`. Chinese translation
  follows section by section; `docs/handbook/` gains `backends.md` and `troubleshooting.md`.
- Documentation no longer carries this cluster's addresses; the registry section describes
  the two tiers without naming hosts.

### Build

- A release can now be rebuilt byte for byte from its own assets. The stamped date is the commit date in
  UTC, read once and shared by both architectures; it used to be `date -u` evaluated inside the
  per-architecture loop, so the arm64 and amd64 binaries of one tag were stamped seconds apart and the
  pair could not both match a rebuild.
- `build-info.txt` records what is needed to reproduce the bytes: version, full commit, tag, build date,
  the exact `ldflags` string, the `GOOS`/`GOARCH` of every asset, the Go toolchain, and the digest of the
  job image. It also carries a `reproduce=` line with the command to run.
- The release job names the toolchain it built with (`go1.23.12` in `library/ci-go:1.23`, digest
  `sha256:4066…`) and fails rather than publishing when the image's Go differs from that pin. The 0.2.0
  assets were built by go1.23.4 on a machine that was not this pipeline, which is why they cannot be
  reproduced from this source.
- `SHA256SUMS` now covers `build-info.txt` as well as the binaries. It cannot cover itself.
- The build lives in `make dist`, which the workflow calls: what the pipeline does and what a person runs
  locally are the same command.

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
