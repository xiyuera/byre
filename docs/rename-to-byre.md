# Renaming to `byre` — what moved, what did not, and the risk accepted

ADR-0004 picked `keypod` on a collision check that was never given the depth its rejects received; the
correction in that ADR lists four same-name software projects, one of them a GPL key-value store in the
same semantic space. Renaming is cheapest while there is one repository and no users.

`byre` was chosen by the maintainer for how it reads: a byre is a cowshed — a small enclosed space an
animal is kept in, which is what a leased instance is.

## The collisions, recorded rather than argued away

The same check applied to every other candidate was applied here too, and it did **not** come back clean:

| prior use | what it is | verdict |
|---|---|---|
| `pjlsergeant/byre` | MIT, Go, single binary: "a comfortable, constrained agent environment around any folder" — it creates a local container around a project directory. Has a site (`getbyre.com`), docs, a brew cask and a Show HN launch | **same domain** — an agent sandbox tool under the same name |
| `halzy/byre` | Rust service bootstrap toolbox, published on `crates.io` as `byre = "0.3"` | the name already occupies a package coordinate |

The maintainer accepted both. What that means in practice, and what follows from it:

- **No import-path collision.** Go module paths are namespaced by repository, so `byre/internal/...` and
  `github.com/pjlsergeant/byre/...` cannot shadow each other. The cost is searchability and confusion for
  a reader who has seen the other tool, not a technical conflict.
- **No trademark position is claimed.** Apache-2.0 §6 grants none, and nothing in this repository asserts
  affiliation with, endorsement by, or origin from any same-named project. Keep it that way: no
  third-party logos, no `®`, no phrasing that implies a relationship.
- **If the confusion becomes real** (issues filed for the other project, package registries mixing them),
  the fallback name is `cranage` — collision-checked clean in the same pass, and no longer than this one.

## What changed

- Go module path: `keypod` → `cranage` → **`byre`**, with every in-module import.
- `Makefile` ldflags target the new path. Not cosmetic: a stale path makes `-X` silently do nothing and
  the binary reports an empty version, which breaks the `/healthz` and release checks.
- ADR-0004 status, this document, the README's standing line.

## What deliberately did NOT change

Runtime identifiers stay as they are for now: namespace `keypod-system`, the Deployment/Service names, the
image name, the binary (`cmd/keypod`, `bin/keypod`), the annotation keys `keypod.io/*`, the four profiles'
`ssh_host`, and the board tenant.

Renaming those needs a cutover window in which a running gateway is replaced, and the board is currently
handing out instances to several agents. The rule from this project's own history applies:
**new → verify → migrate → retire**, with the old path left working until the new one is proven.

## The cutover, when it happens

1. New namespace `byre-system`, new Deployment/Service names, annotations `byre.io/*`, new image name —
   deployed **alongside** the running gateway, on a second port.
2. Point one non-`default` profile at it and verify a full session (identity, lease, `-L`, scp).
3. Move the remaining profiles one at a time; the old gateway serves whoever is left.
4. Retire `keypod-system` and the old image tags; keep the old repository archived.
5. Only then rename the board tenant and the annotation keys — those need a data migration because live
   objects carry the old annotation.

Steps 1–2 are additive and reversible; step 4 is the only irreversible one.

## Where the first agent-filed issue lives

Issue #1 ("rotation: host key replacement has no dual-trust window", author `hermes-reviewer`) was filed
against the pre-rename repository. The rotation work itself is tracked by ADR-0006 and the v0.1.3
hardening card; new issues belong on this repository.
