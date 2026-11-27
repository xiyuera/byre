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

Those four are about the code. How contributors treat each other is
[`CODE_OF_CONDUCT.md`](code-of-conduct.md) — Contributor Covenant 2.1, with a Chinese summary at the
end of it.

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
named `hermes-<role>`, and the branch drops the `hermes-` prefix:

| account | branch prefix | example |
|---|---|---|
| `hermes-eng` | `eng/` | `eng/lease-row-phase` |
| `hermes-ops` | `ops/` | `ops/registry-two-tiers` |
| `hermes-reviewer` | `reviewer/` | `reviewer/pr-checklist` |
| `hermes-legal` | `legal/` | `legal/licence-audit` |
| `hermes-research` | `research/` | `research/competitor-scan` |
| `xiyuera` (owner) | `ops/` or the role the work belongs to | `ops/git-merge-gate` |

The owner has no `xiyuera/` prefix: the account that pushes is not always the
account that owns, and a branch pushes as whoever runs it. This PR is the
example — owned by `xiyuera`, pushed from `ops/git-merge-gate`.

Account names, credential files, token scopes and the per-role commit identity
are recorded in the repository's agent-contributor notes;
this file covers the branch and the merge, that one covers who is behind them.

The point of the prefix is that `git branch -r` answers "who is working on
what" without a lookup — so it has to be an account that can push, not a
category like `docs/` that reads like a role but cannot own a commit.

This is a convention, not a gate: nothing stops you pushing `test/whatever`.
When this was written, 20 of 21 work branches followed it — `eng/` 14, `ops/` 6 —
and the one that did not was `test/ci-probe`, a branch pushed to exercise the
runner. It should have been `ops/ci-probe`; it was not worth a force-push to
rename, and it is the only exception you will find.

`dev` is not a branch here. If a second integration branch is ever needed it
gets the same protection on the day it is created — an unprotected `dev` would
just be a second `main` with less review.

### The four required checks

The names below are the job names. The branch protection compares **exact
strings**, and each job publishes two contexts — `ci / test (pull_request)` and
`ci / test (push)` — so the protection must list the `(pull_request)` variants or
the pull request will wait for a check that never arrives.

| check | what it means |
|---|---|
| `ci / test` | `make fmt vet`, `go test ./...`, `go test -race ./...` (the whole module, not a subset), `make build` |
| `ci / manifests` | `make render` — the kustomize tree renders, and the job's earlier `kustomize version` step is what makes that meaningful: `make render` prints `skipped` and exits **0** when kustomize is absent, so run `kustomize version` yourself before trusting a local green. It does **not** schema-validate the output |
| `ci / dco` | every commit the change **adds** carries a matching `Signed-off-by` — `base..HEAD` on a pull request, `before..sha` on a push |
| `ci / i18n` | the translation manifest matches the files it indexes |

A red check is information, not an obstacle: read the job log before rerunning.

### Every commit needs `Signed-off-by`

`git commit -s`. The `dco` job compares each commit's trailer against *that commit's own* author and
reads only the range the run added — a merge commit is created when the button is pressed and carries
no trailer, so the run for `main` fails even when every commit on the branch was signed. **Use
rebase**, which adds no commit of its own.

The full rule — the script the job runs, what each of the three merge strategies does to `main`'s
history, and why the pre-policy merge commits are not a counter-argument — is under [Developer
Certificate of Origin](#developer-certificate-of-origin).

### Who reviews

The forge requires one approval, from any account with write access that is not
the author. That is all the machine checks — it has no notion of who is a
reviewer. The convention on top of that:

* `hermes-reviewer` reviews changes that touch behaviour; its approval is the
  one that matters.
* `hermes-eng`, `hermes-ops` and the owner may approve when `hermes-reviewer`
  is the one who wrote the change.

Any account with write access can check that part:

```sh
curl -sS -H "Authorization: token $TOKEN" \
  "$HOST/api/v1/repos/byre/byre/branches/main" | grep -o '"required_approvals":[0-9]*'
# required_approvals:1     and separately: user_can_push:false
```

What the forge does **not** enforce is *who* reviews — the approvals whitelist is
off. That switch lives in `/branch_protections/main`, which needs an owner or
admin token (`write:repository` gets 403 `user should be an owner or have admin
permission of the repository`). A reviewer therefore cannot read it, which is
exactly why the role division below is written as convention and nothing in this
file depends on it.

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

### What `main`'s history already looks like

18 merge commits are reachable from `main` and 17 of them carry no
`Signed-off-by`. They are not an argument against the rule above; they are what
the rule was added because of. The `dco` job was already in `ci.yml` when two of
them (`6f54b7a`, `9ed7076`) landed at 17:40 and 17:41 — merged with the button,
before the protection existed. They are not rewritten now because rewriting
`main` again would invalidate another batch of published hashes for no benefit
(`docs/tag-map.md` records what that cost the first time).

More than one commit after the policy is in the same position, and not all of them are
merge commits. Counted over everything reachable from `main` — the same all-reachable
basis the counts above use — 39 commits between the policy commit and `9352915` carry no
`Signed-off-by`: 16 merge commits and 23 ordinary ones. On the first-parent line, which is
what `git log --first-parent` shows, 20 carry none: 13 merge commits and 7 ordinary ones.

There is no window in which the policy existed and the check did not: the `dco` job
entered `ci.yml` in `94d5b52`, the same commit that wrote the policy, so every commit in
those counts landed with the job already in the file. The counts say nothing about why any
single one of them was let through; that is a per-commit API lookup, not a fact this file
can assert.

What gets a paragraph here is narrower than "every unsigned commit", because the only
reason to write one is that rewriting the commit would cost something that already exists
outside the repository. A tag does exactly that: the release built from the commit records
a hash, so a rewrite invalidates both the hash and the record. Three commits after the
policy are named by a tag — `0ee8c66` (`v0.2.0`), `76489ee` (`v0.3.1`) and `9352915`
(`v0.3.3`) — and each has a paragraph below. The other 36 are counted here rather than
narrated: 16 merge commits and 20 ordinary ones, none of them named by a tag.

`0ee8c66` is `v0.2.0`, the first release built after the policy. Its `build-info.txt` names
`d5b87cc`, a commit the 2026-11-17 rewrite removed, which leaves the tag and `0ee8c66` as
the only thing the release can be rebuilt from (`docs/tag-map.md` records what that rewrite
cost). Rewriting it would leave `v0.2.0` naming nothing.

`76489ee` is `v0.3.1`, landed at 18:08 on 2026-11-17 as an ordinary commit with one
parent, `9ed7076`, and no trailer. Its own push run never executed `dco`: run 155 was
cancelled by the next push with all four jobs at `steps=0`, which Gitea reports as
`failure` at the commit-status layer with the description `Canceled after 0s`. Reading only
that layer would say the gate had run and rejected it; the gate never ran. The missing
trailer is what makes it an exception, not a red run — unlike `9352915` below, whose run
243 did execute `dco` and failed on `missing or mismatched Signed-off-by`. The reason it is
not rewritten is the one above: the release assets of
`v0.3.1` were built from the tag, and moving the tag onto a rewritten commit would
republish a release whose contents changed while its name did not.

The last of the three, `9352915`, is the squash of pull request #26 on 2026-11-18. It is
authored and committed by `hermes-ops`, and its message carries `Co-authored-by` and no
`Signed-off-by`, so its push run fails `dco`. The squash was pressed by `hermes-ops`, the
account that wrote the branch, at 01:22:28 — the same second the reviewing account's
rebase request returned HTTP 500. The content is the head that was approved:
`9352915^{tree}` and `f3360da^{tree}` are equal, so nothing in the change is in question.

Three facts decide what to do with it. Rewriting `main` would invalidate another
batch of published hashes, which is the cost the paragraph above already refused
to pay. Rewriting is also the owner's action and not a contributor's, because it
means lifting the branch protection: `hermes-ops` reads `permissions.admin=false`
on this repository, so the account that squashed cannot rewrite what it squashed.
And leaving it costs nothing at run time — a push run bases `dco` on the previous
tip, so the next push to `main` is checked from `9352915` onwards and turns the
tip green without this commit changing. So it stays, registered here.

Every number in this section comes from the range that ends at `9352915`, so it stays
reproducible after `main` moves:

```sh
pol=$(git log -S 'Developer Certificate of Origin' --format=%H -- CONTRIBUTING.md | tail -1)
count_unsigned() { git rev-list "$@" | while read c; do
  git log -1 --format=%B "$c" | grep -qi '^Signed-off-by:' || echo x; done | wc -l; }

count_unsigned "$pol..9352915"                        # 39
count_unsigned --merges "$pol..9352915"               # 16
count_unsigned --no-merges "$pol..9352915"            # 23
count_unsigned --first-parent "$pol..9352915"         # 20  (13 merge + 7 ordinary)

# the three commits the paragraphs above are about: unsigned, after the policy, named by a tag
git tag | while read t; do
  c=$(git rev-list -n1 "$t")
  git merge-base --is-ancestor "$c" "$pol" && continue
  git log -1 --format=%B "$c" | grep -qi '^Signed-off-by:' || echo "$t $c"
done                                                  # v0.2.0 / v0.3.1 / v0.3.3
```

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

### If `ci / test` fails on a *download*

The job's `GOPROXY` comes from the repository secret `GOPROXY` (the value is not
in this file because a working proxy is usually an internal host). Unset, Go falls
back to `proxy.golang.org`, which in this cluster intermittently returns `EOF`
part-way through the larger module zips:

```
internal/backend/kubernetes.go:15:2: k8s.io/api@v0.31.4:
  Get "https://proxy.golang.org/k8s.io/api/@v/v0.31.4.zip": EOF
internal/backend/kubernetes.go:24:2: k8s.io/client-go@v0.31.4:
  Get "https://proxy.golang.org/k8s.io/client-go/@v/v0.31.4.zip": EOF
... 19 such lines over 5 modules: k8s.io/api (1), apimachinery (8),
    client-go (7), gopkg.in/yaml.v3 (1), golang.org/x/crypto (2)
```

That is a network failure, not your change: the same commit has passed and failed
on consecutive runs. **Re-running may pass**, which is why this was easy to
misdiagnose as flaky code.

**The way to tell a fix from luck is the download window, not red-to-green.** In
the job log, the block of `go: downloading …` lines is the whole dependency fetch;
time it:

| job | GOPROXY | download window | result |
|---|---|---|---|
| 623 | unset | 368.4 s | success |
| 639 | unset | 86.2 s | success |
| 643 | unset | 30.5 s | success |
| 627 | unset | 5.1 s, then `EOF` | **failure** |
| 655 | secret | **2.4 s** | success |

A two-order-of-magnitude difference means the proxy took effect. Success alone
does not — an unset `GOPROXY` also succeeds when the network happens to behave.

#### Setting it

`GET`/`PUT` on `/actions/secrets` need an **owner or admin** token;
`write:repository` gets `403 user should be an owner or have admin permission`.
So a contributor cannot check or set this, and should not try — use the download
window above instead, and ask an owner if it needs changing.

```sh
# owner/admin only
curl -sS -X PUT -H "Authorization: token $TOKEN" -H 'Content-Type: application/json' \
  -d '{"data":"<proxy-a>|<proxy-b>|direct"}' \
  "$HOST/api/v1/repos/byre/byre/actions/secrets/GOPROXY"
```

**Use `|`, not `,`.** Go's rule (`go help goproxy`, and
`src/cmd/go/internal/modfetch/proxy.go`): a comma falls back to the next entry
only after a **404 or 410**; a pipe falls back after **any** error, including the
`EOF` and timeouts this section is about. With commas, this exact failure would
try the first proxy, die, and never reach the second — the fix would look applied
and change nothing. Measured both ways against a deliberately dead first proxy:
commas exit 1, pipes reach the second source and succeed.

Two things this cannot fix:

* **A pull request from a fork gets no secrets**, so `GOPROXY` is empty and the
  `proxy.golang.org` failure is back. Submit from a branch in this repository, or
  expect the occasional red.
* **The proxy URL appears in CI logs when it fails** (`Get "https://<host>/…"`),
  and job logs are readable by repository members. A secret keeps the address out
  of the tree, not out of a failure message.

`make fmt` rewrites files in place (`gofmt -l -w .`) and exits 0 whether or not it
changed anything — it formats, it does not check. `make lint` is a **local** gate,
not a machine one — CI runs `fmt vet`, `test`,
`-race ./...`, `build`, `render`, `dco` and `i18n`, and golangci-lint is not among
them. A clean CI does not mean lint is clean.

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

Sign-off is a statement that the contribution may be distributed under Apache-2.0 (the licence
already grants the inbound right in §5); it is the record, not the permission.

### What the check does

CI runs this over every commit a run added:

```sh
base="${GITHUB_BASE_SHA:-${GITHUB_BEFORE:-HEAD~1}}"
bad=0
for c in $(git rev-list "$base..HEAD"); do
  author="$(git log -1 --format='%ae' "$c")"
  if ! git log -1 --format='%B' "$c" | grep -qi "^Signed-off-by: .*<$author>"; then
    echo "missing or mismatched Signed-off-by: $(git log -1 --format='%h %s' "$c")"
    bad=1
  fi
done
[ "$bad" = 0 ]
```

Rewriting a commit can drop the trailer: `git commit --amend` keeps the message, but a rebase that
rewords it does not, so re-check `git log -1 --format=%B` after either.

A commit passes when its message carries a `Signed-off-by:` line naming the address that same commit
was authored with. `-s` writes that line from the author identity, so what this mostly catches is not
a forgotten flag but an identity that answers to nobody: `x <x@x>`, the shared placeholder account
the first commits used, or a branch whose commits were made under two different addresses. Commit as
the role account described in the agent-contributor notes, let
`-s` write the trailer, and do not override the identity with `-c user.name=` or `-c user.email=`.

The range is what the run added, not the whole history. A pull request run is based on the merge
target, so it checks the branch's own commits; a push run is based on the previous tip of the branch
being pushed, so on `main` it checks the push itself.

### Merging

A merge commit is a new commit, so it is checked like any other. The commit the forge's merge button
writes is authored by whoever clicked it and carries no `Signed-off-by`, so the run for `main` fails
even when every commit on the branch was signed. On 2026-11-17, seventeen of the eighteen merge
commits in `main` failed for exactly this reason.

`main` is protected and a contributor's push to it is refused (`user_can_push: false`), so a change
does not land by merging locally and pushing the result: what decides the run for `main` is the
merge strategy chosen on the pull request. An operations account that lifts the protection for an
exceptional change writes that commit itself, and it has to carry its own sign-off, because the run
for `main` checks it like any other commit.

- **Rebase then fast-forward** adds no commit of its own: the branch commits arrive with their
  authors and their trailers, and the range the run checks is signed commits only. Use this one.
- **Squash** writes one new commit, authored by whoever squashes it, so it passes the gate when that
  commit's message carries a sign-off naming the same author; a squashed message that only carries
  the contributors' trailers does not.
- **A plain merge** writes the unsigned merge commit described above and turns `dco` red on `main`.

Branch protection only gates the four `(pull_request)` contexts — `ci / test`, `ci / dco`, `ci / i18n`,
`ci / manifests`. So "rebase, don't merge" keeps `main`'s history signed; it does not stop anyone from
pressing the merge button. What catches that is the push run for `main`, after the fact.

A merge commit that carries its own sign-off would pass, but making one means writing to `main`
directly, which the branch protection forbids — that is an operations action, not a part of a normal
contribution.

### Before the policy

Sign-off became policy in the commit that added the licence and NOTICE files (find it with
`git log -S 'Developer Certificate of Origin' -- CONTRIBUTING.md`). The 31 commits before it are all
unsigned: 24 by `keypod contributors`, the placeholder the project used before the rename, 6 by the
owner account, and 1 by an early reviewer account. None of the last seven is a role account in the
sense the per-role table uses — they predate the naming rule. A sign-off is a statement the author makes, so it cannot be made
on their behalf now. A
build from before the policy is recognisable from the `commit` recorded in its `build-info.txt`.

That history matters at run time, not just for tidiness. The counts and the reason they are not being
rewritten are in [`What main's history already looks like`](#what-mains-history-already-looks-like)
above; what follows from them here is that a branch which merges `main` into it pulls the unsigned
history into the range and fails. Rebase onto the merge target instead, and check what a pull request
will see before pushing:

```sh
git rev-list --count "origin/main..HEAD"
```

This counts the branch's own commits — what a pull-request run checks when its base is the branch
point.

The policy starts at that commit; a commit that lands after it without a sign-off is an
exception rather than a case the policy never covered, and what this file does with one is
decided by whether a tag names it. Every such commit that a tag names gets a paragraph in
[`What main's history already looks like`](#what-mains-history-already-looks-like) saying
what landed, how it happened and why it is not rewritten — three of them exist as this is
written, and a fourth would need the fourth paragraph. The ones no tag names are counted in
that section, with the command that reproduces the count, and are not narrated one by one.
