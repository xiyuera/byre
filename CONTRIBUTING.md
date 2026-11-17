# Contributing

## Ground rules

1. **The client contract is sacred.** Anything that could make one identity's session land on two
   instances (or two identities share one) is a release blocker, not a trade-off.
2. **No third-party services.** Standard library, `golang.org/x/crypto/ssh`, the official Kubernetes
   client, and small libraries that carry no runtime process (YAML parsing, for example). Proxies,
   gateways, caches and sidecars are out.
3. **No environment specifics.** Hostnames, namespaces, image tags, key paths and identity names belong
   in configuration and examples, never in source or docs. Examples use placeholders.
4. **Fail fast.** A configuration error must stop startup with every violation listed. Silent defaults
   are how "it worked yesterday" bugs start.

## How a change reaches `main`

`main` is protected by the forge, and the protection applies to **every**
account including the owner: `enable_push: false`, `enable_force_push: false`.
A direct push fails for everyone. This is deliberate — a rule the privileged
account can skip is a habit, not a rule.

```
<account>/<slug>  →  PR  →  four checks green  →  one approval  →  merge
```

### Branches

`main` is the only long-lived branch. Work branches are named for who and what:
the first segment is the account doing the work. Accounts on this forge are
named `hermes-<role>`, and the branch drops the `hermes-` prefix — which is what
every branch that exists today already does:

| account | branch prefix | example |
|---|---|---|
| `hermes-eng` | `eng/` | `eng/lease-row-phase` |
| `hermes-ops` | `ops/` | `ops/registry-two-tiers` |
| `hermes-reviewer` | `reviewer/` | `reviewer/pr-checklist` |
| `hermes-legal` | `legal/` | `legal/licence-audit` |
| `hermes-research` | `research/` | `research/competitor-scan` |

The point of the prefix is that `git branch -r` answers "who is working on
what" without a lookup — so it has to be an account that can push, not a
category like `docs/` that reads like a role but cannot own a commit.

`dev` is not a branch here. If a second integration branch is ever needed it
gets the same protection on the day it is created — an unprotected `dev` would
just be a second `main` with less review.

### The four required checks

| check | what it means |
|---|---|
| `ci / test` | `make fmt vet`, `go test ./...`, `go test -race ./...` (the whole module, not a subset), `make build` |
| `ci / manifests` | `make render` — the kustomize tree renders. It does **not** schema-validate the output |
| `ci / dco` | every commit in the push range carries a matching `Signed-off-by` |
| `ci / i18n` | the translation manifest matches the files it indexes |

A red check is information, not an obstacle: read the job log before rerunning.

### Every commit needs `Signed-off-by`

`git commit -s`. The `dco` job reads trailers over the range the push
introduces and requires the trailer to match the author. Two consequences:

* **Use rebase, not merge commits and not squash.** Gitea's merge button
  produces a merge commit whose message carries no trailer. A squash produces a
  single new commit authored by whoever pressed the button, so the trailers it
  concatenates no longer match that commit's own author — and `dco` compares each
  trailer against *that commit's* author (`Signed-off-by: .*<$author>`). Only a
  rebase keeps the commits, their authors and their trailers intact.
* Rewriting a commit can drop the trailer. If you amend, re-add it.

### Who reviews

The forge requires one approval, from any account with write access that is not
the author. That is all the machine checks — it has no notion of who is a
reviewer. The convention on top of that:

* `hermes-reviewer` reviews changes that touch behaviour; its approval is the
  one that matters.
* `hermes-eng`, `hermes-ops` and the owner may approve when `hermes-reviewer`
  is the one who wrote the change.

(The approvals whitelist is off: Gitea 1.27 accepts `enable_approvals_whitelist`
but stores an empty username list, which would mean *nobody* can approve.)

The reviewer answers three questions in a comment, not "looks good":

1. Does the change do what its description says, and is that verifiable from
   the diff?
2. Does it break anything currently true — config keys, documented behaviour,
   the deployment tree?
3. Is anything unverified: a claim with no command behind it, a number with no
   source?

### Tags and releases

A tag is created from a commit already on `main`. Pushing a `v*` tag is itself
what triggers the release workflow (`on: push: tags`), so "wait until the release
job passes, then tag" is not executable — nothing runs before the tag exists. The
workable order is: merge → push the tag → watch the job → if it fails, fix
forward and move the tag rather than leaving a half-published asset set behind.

Moving a tag takes two commands, because `git tag -f` only rewrites the local
ref — the remote keeps pointing at the old commit and the workflow does not run
again:

```sh
git tag -f v0.3.2 <new-commit>
git push --force origin v0.3.2      # the local move alone changes nothing remotely
```

A tag is never a route to get code onto `main`.

Release assets must be rebuildable from the tag they name. If history is ever
rewritten, every published `build-info.txt` that names a commit is invalidated
and the fixed point becomes the **tag**, not the commit hash — see
`docs/tag-map.md` for what that looked like here, and do not edit published
assets in place.

### The two operations that cannot go through a PR

Both act on the repository itself, so both require an explicit decision
recorded outside this file (a ticket, a task, a comment thread), the protection
lifted for the duration, and the protection restored immediately after.

* **Rewriting history.** A rewritten `main` invalidates every recorded commit
  hash; the tag map is the deliverable.
* **Restoring a deleted branch** whose content is not reachable from `main`.

In both cases the evidence to leave behind is a push that *fails* — proof the
protection is back on.

### After merging

Delete the branch. Fifteen merged branches were still on the forge when this was
written; a branch whose content is already reachable from `main` is only
clutter, and it makes `git branch -r` useless for spotting what is actually in
flight. Delete it as part of the merge, not "later".

If the history was ever rewritten, the old-to-new **tag map** belongs in
`docs/tag-map.md` — inside the repository, not in an operator's notes. The
mapping is the only thing that keeps a published `build-info.txt` traceable once
the commit hash it names stops existing.

### When a task board spawned you

The branch, the PR and the review are part of delivering the task. A task that
ends with "committed to main" has not delivered: it left the work where nobody
could review it and where the next change overwrites it. Open the PR, put its
number in the task, and let the review loop close it.

## Before opening a PR

```sh
make fmt vet test
make lint          # uses golangci-lint when installed
go test -race ./...
```

## Adding a backend driver

1. Implement `internal/backend.Backend` (`Acquire`, `Release`, `List`, `Adopt`).
2. Extend `internal/config/config.go` validation with the new `provision` value and its required
   fields — unknown values must keep failing.
3. Add unit tests with a fake API client (no live cluster in unit tests).
4. Add an `examples/<driver>/` directory with a minimal, placeholder-filled manifest.
5. Document it in `docs/handbook/architecture.md` §5 and, if it needs new tunables, in
   `docs/handbook/backends.md` and `docs/handbook/operations.md`.

## Changing the lease semantics

Anything touching `ttl`, `grace`, renewal or adoption needs an ADR (see `docs/adr/`) and a test that
fails on the previous behaviour. Two behaviours that must never regress:

- a reconnect within `grace` returns the *same* instance;
- an expired lease never hands its instance to a different identity.

## Commit messages

`<area>: <imperative summary>` — e.g. `lease: renew on channel open, not only on data`.
Body: why, then what changed, then how it was verified. Reference the ADR when one applies.

## Reporting a bug

Include: byre version (`byre -version`), the relevant log lines (identity prefixes are fine,
full keys never are), the provision value, and whether the client uses `ControlMaster`.

## Security

Do not open public issues for a vulnerability. Send a description and reproduction steps to the
maintainers' private address listed in `SECURITY.md` once it exists (M5); until then, contact a
maintainer directly.

## Developer Certificate of Origin

Every commit must be signed off (`git commit -s`), certifying the DCO 1.1:

```
Developer Certificate of Origin
Version 1.1

Copyright (C) 2004, 2006 The Linux Foundation and its contributors.

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I have the right to submit it under the
    open source license indicated in the file; or
(b) The contribution is based upon previous work that, to the best of my knowledge, is covered under an
    appropriate open source license and I have the right under that license to submit that work with
    modifications, whether created in whole or in part by me, under the same open source license (unless
    I am required to submit under a different license), as indicated in the file; or
(c) The contribution was provided directly to me by some other person who certified (a), (b) or (c) and I
    have not modified it.
(d) I understand and agree that this project and the contribution are public and that a record of the
    contribution (including all personal information I submit with it, including my sign-off) is
    maintained indefinitely and may be redistributed consistent with this project or the open source
    license(s) involved.
```

CI rejects a pull request whose commits lack `Signed-off-by:` matching the author. Sign-off is a
statement that the contribution may be distributed under Apache-2.0 (the licence already grants the
inbound right in §5); it is the record, not the permission.

**History:** commits made before the DCO was adopted carry no sign-off and cannot be signed
retroactively. They are listed in the release notes as pre-DCO.
