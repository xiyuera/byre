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

`.gitea/workflows/release.yml` runs on a `v*` tag and does nothing but supply inputs to `make dist`:

1. `make dist` builds static `linux/arm64` and `linux/amd64` binaries, `CGO_ENABLED=0`, with one build
   date for both — the commit date in UTC, read once;
2. the same target writes `build-info.txt` (version, commit, tag, build date, the exact `ldflags`, the
   `GOOS`/`GOARCH` of every asset, the Go toolchain, the digest of the job image, and a `reproduce=`
   line) and `SHA256SUMS` over the binaries and `build-info.txt`;
3. create the release on Gitea through the API and upload those four files;
There is no step 4. `release.yml` contains no GitHub API call, and it should not be described as if it
did: publishing to a second host is a manual mirroring act, not part of a release.

## Rebuilding a release

The assets are an input, not a result: `build-info.txt` holds everything needed to rebuild the bytes.

```sh
git clone <repository> && cd byre
git checkout <the commit= value from build-info.txt>
make dist VERSION=<version> COMMIT=<commit_short> BUILD_DATE=<BuildDate> \
          CI_IMAGE=<ci_image> CI_IMAGE_DIGEST=<ci_image_digest>
cd dist && sha256sum -c SHA256SUMS
```

Two builds of one commit produce identical bytes, which is why the build date is the commit date rather
than the clock on the builder. Anything that changes the bytes must change a field `build-info.txt`
records: the source, the flags, or the compiler. The job image is pinned in `release.yml` and the job
fails when the image's Go differs from the pin, so a compiler change cannot enter a release unnamed.

`SHA256SUMS` covers every other asset but not itself — a checksum file cannot contain its own digest.

## Publishing to GitHub

Gitea is the source of truth; GitHub is a public mirror.

- **Code and tags** travel by Gitea's *push mirror* (Settings → Repository → Mirror Settings). It syncs
  branches, tags and commits only — issues, PRs and releases are **not** mirrored, and a *pull* mirror
  deletes releases created on the mirror side.
- **Release assets are not mirrored.** The pipeline uploads to Gitea only; copying a release to GitHub
  is a manual step, and undocumented automation is worse than none: `gh release create <tag> \
  --notes-file <changelog section> dist/*` after the tag exists on both sides.
- **Nothing flows back.** Pull requests are accepted on Gitea only; the GitHub repository stays read-only so
  there is one place where a review happened.

## What the repository publishes

Source, and for each release: two static binaries, `SHA256SUMS`, `build-info.txt`, and the changelog entry.
Container images are published to a registry separately and referenced by digest — never by mutable tag.
No `go get`: the module path is deliberately bare (see README).
