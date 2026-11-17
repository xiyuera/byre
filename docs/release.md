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

`.gitea/workflows/release.yml` runs on a `v*` tag:

1. build static `linux/arm64` and `linux/amd64` binaries, `CGO_ENABLED=0`;
2. stamp version/commit/date through ldflags (`make build` already does this);
3. write `SHA256SUMS` and a small `build-info.txt` (`go version`, commit, tag, workflow run);
4. create the release on Gitea and upload those assets;
5. if a `GH_TOKEN` secret exists, mirror the same release to GitHub.

Steps 1–4 are what "a release" means here. Step 5 is optional on purpose: publishing to a second host is a
mirror, not a second source of truth.

## Publishing to GitHub

Gitea is the source of truth; GitHub is a public mirror.

- **Code and tags** travel by Gitea's *push mirror* (Settings → Repository → Mirror Settings). It syncs
  branches, tags and commits only — issues, PRs and releases are **not** mirrored, and a *pull* mirror
  deletes releases created on the mirror side.
- **Release assets** travel from the pipeline (step 5 above), because the mirror cannot carry them.
- **Nothing flows back.** Pull requests are accepted on Gitea only; the GitHub repository stays read-only so
  there is one place where a review happened.

## What the repository publishes

Source, and for each release: two static binaries, `SHA256SUMS`, `build-info.txt`, and the changelog entry.
Container images are published to a registry separately and referenced by digest — never by mutable tag.
No `go get`: the module path is deliberately bare (see README).
