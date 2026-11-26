# Publishing to GitHub

`main` is mirrored to `github.com/xiyuera/byre` by `.github/workflows/mirror.yml`. Nothing else is.

## Why not Gitea's push mirror

Gitea's built-in push mirror is `git push --mirror`. It publishes every branch the moment it is
pushed, so a work-in-progress branch is public before anyone has reviewed it — and stays public
until it is deleted. The public repository is a release channel, not a scratch area.

The push mirror was removed on 2026-11-20. The workflow below replaced it.

## Why the workflow lives in `.github/workflows/`

Three placements were tried:

| Placement | Result |
|---|---|
| `.gitea/workflows/mirror.yml` | A `.gitea/**`-only diff does not trigger CI at all (Gitea's native behaviour). But branch protection on `main` requires four `ci/*` checks, so such a pull request can never satisfy them — **every PR touching the file deadlocks**. |
| `.github/workflows/` + `paths-ignore: [".gitea/**"]` on ci.yml | Worse: on Gitea 28.4 a pull request that also modified `ci.yml` produced `total_count: 0` checks — not "skipped", nothing. |
| `.github/workflows/` + in-script guard (current) | Works. CI runs, PR merges, and github itself exits in the first step. |

The cost of the current placement is that github runs the job too. It costs about five seconds and
cannot push anywhere: the first line of the script checks `GITHUB_SERVER_URL` and exits.

## The secret

`MIRROR_GITHUB_TOKEN`, a fine-grained token with `contents: write` on `xiyuera/byre`.

It is **not** named `GITHUB_*`: Gitea rejects secret names with that prefix so they cannot shadow
its built-in variables (the API returns 400 with `name cannot start with 'GITEA_' or 'GITHUB_'`).

## What the job does

1. Exits immediately when it finds itself on github.
2. Exits successfully when the secret is absent — a missing mirror token must not fail an otherwise
   good commit.
3. Bare-inits a scratch repository, fetches `main` from Gitea with the per-job token, and pushes that
   one ref to github.

A bare repository with no working tree means there is no stale local branch that could be pushed
instead of the one just fetched.
