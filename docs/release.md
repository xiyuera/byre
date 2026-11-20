# Releasing

## When to cut a version

Not on a calendar. A version exists when a user-visible thing changed:

| change | bump | example |
|---|---|---|
| a bug a user could hit | patch | host key changed on every restart |
| a new config field, provision shape, flag or behaviour | minor | `provision: workloads` |
| a breaking change to the config file, annotations or metrics | major (or `v0.x` minor while pre-1.0) | — |

Two patches in one hour is normal if two bugs are found in one hour — the problem the first releases had
was not the interval, it was that the tag said nothing about what changed. `CHANGELOG.md` is the answer:
an entry appears under `Unreleased` when the change lands, and the tag is cut from the entry, not the other
way round.

A release is not "a passing CI run". It needs, in the release notes:

1. the changelog section verbatim,
2. the acceptance record it was verified against (`docs/acceptance/…`),
3. the image digest, not just the tag.

## How the pipeline works

`.github/workflows/release.yml` runs on a `v*` tag. It is one workflow serving two forges, so the
steps differ only where the forges genuinely differ — never by duplicating the file.

1. **Checkout the tagged commit**, not the default branch. `git clone` without a ref lands on
   `main`, which is how a `pull_request` run once spent days testing the wrong tree.
2. **Build both binaries**: `linux/arm64` and `linux/amd64`, `CGO_ENABLED=0`, plus `SHA256SUMS`.
   The smoke check (`--version`) runs only the artifact matching the runner's own architecture:
   a cross-built ELF cannot start, and `Exec format error` (exit 126) takes the whole job down
   for a step whose only job was to prove the native one runs.
3. **Push the image.** `vars.REGISTRY` decides where. Unset on GitHub means `ghcr.io`, and the
   job token authenticates it because the workflow declares `permissions.packages: write`.
   Unset on Gitea means this forge has no registry wired up — a Gitea token cannot authenticate
   to GHCR at all — so the step prints why and exits 0 instead of failing a tag build whose
   binaries are fine. Set `vars.REGISTRY` on either side to override.
4. **Create the release** on whichever forge is running, uploading the binaries, `SHA256SUMS`,
   `build-info.txt` and `images.txt`. The GitHub half uses `gh release create`; the Gitea half
   uses the REST API, because `tea` is not guaranteed on a job pod.

There is no `uses:` anywhere in either workflow: Gitea ignores step-level `if:` and executes the
action anyway, so a forge-guarded `uses: actions/setup-go` runs on a runner that cannot reach
GitHub. Both conditions and the toolchain are shell.

### Compatibility of the toolchain

The job image carries Go and docker; when `go` is absent the step installs the version `go.mod`
asks for and appends it to `$GITHUB_PATH`. Module downloads honor `secrets.GOPROXY` with a
`https://proxy.golang.org,direct` fallback — the `||` matters because an empty `GOPROXY` means
*off* to the go command, not "use the default".

## Cutting a release

`scripts/release.sh vX.Y.Z` does the whole thing and verifies what a user touches:

1. tree clean, on `main`, level with `origin/main`; `CHANGELOG.md` has that version's section;
   the commit's push-event CI is green;
2. annotated tag, **idempotent** — same commit reuses it, a different commit is refused rather
   than silently moved;
3. push the tag to **both** forges and read both back, comparing tag object IDs. Gitea's push
   mirror propagates a tag *deletion* but not a tag *re-creation*, so a re-cut tag exists on the
   source and nowhere else; a wait loop then looks like "still building" instead of
   "will never start", which is exactly how one release attempt lost half an hour;
4. bounded waits on each forge's release run, with the failure named;
5. verify release assets on both forges and that the image manifest answers `200` to an
   **anonymous** request.

Pushing the tag by hand — `git push origin vX.Y.Z` — is what makes step 3 fail later. There is
no supported path that skips it.

## Rebuilding a release

The assets are an input, not a result: `build-info.txt` holds everything needed to rebuild the bytes.

If the `commit=` line names a commit that is no longer in the repository, the
history was rewritten after that release: see [tag-map.md](tag-map.md) for the
current tag pointers and how far the old and new trees were compared.

```sh
git clone <repository> && cd byre
git checkout <the commit= value from build-info.txt>
make dist VERSION=<version> COMMIT=<commit_short> BUILD_DATE=<BuildDate> \
          CI_IMAGE=<ci_image> CI_IMAGE_DIGEST=<ci_image_digest>
cd dist && sha256sum -c SHA256SUMS
```

Two builds of one commit produce identical bytes, which is why the build date is the commit date rather
than the clock on the builder. Anything that changes the bytes must change a field `build-info.txt`
records: the source, the flags, or the compiler.

`SHA256SUMS` covers every other asset but not itself — a checksum file cannot contain its own digest.

## What the two forges do differently

Gitea is the source of truth; GitHub is a public mirror.

| | Gitea | GitHub |
|---|---|---|
| role | source, reviews happen here | public mirror, read-only |
| code and tags | pushed directly | receive a Gitea *push mirror* |
| releases/assets | pipeline creates them | pipeline creates them |
| the image | skipped unless `vars.REGISTRY` is set | `ghcr.io`, job token, no config |

Two mirror behaviours worth knowing before debugging a "release never ran":

- **A deleted ref replicates; a re-created one may not.** A tag deleted and re-cut exists on the
  source and not on the mirror. Nothing reports an error — the mirror's own `last_error` stays
  empty, because nothing failed; there was simply nothing to send.
- **Issues, PRs and releases do not mirror at all.** Only refs travel. A release created on Gitea
  is not a release on GitHub, which is why the workflow creates one on each side it runs on.

## What the repository publishes

Source, and for each release: two static binaries, `SHA256SUMS`, `build-info.txt`, `images.txt`, and the
changelog entry.
Container images are published to a registry separately and referenced by digest — never by mutable tag:
`make image-push` records the digest the registry assigned in `dist/images.txt`, the release publishes
the digest the tag resolved to as `images.txt`, and `make image-verify` fails when the running pod is
neither that digest nor that registry.
No `go get`: the module path is deliberately bare (see README).
