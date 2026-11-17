# Renaming to `cranage` — what moved and what did not

ADR-0004 selected `keypod` on a collision check that was never done to the same depth as the rejects.
The correction in that ADR lists the four same-name software projects it missed. This document records
the rename and, more importantly, **where it stops**.

## Why `cranage`

Collision-checked the same way the original alternatives were (GitHub-wide, `pkg.go.dev`, npm, PyPI,
Docker Hub) plus the queries that would surface a same-domain tool:

| candidate | verdict |
|---|---|
| `stile` | rejected — `liamwh/stile` ("capability-oriented secret broker"), `CosmoGao/stile` ("lightweight bastion for SSH and RDP"): two projects in *this* domain |
| `byre` | rejected — `pjlsergeant/byre`, "a comfortable, constrained agent environment" |
| `hutch` | rejected — `ruby-amqp/hutch`, `K-Arthur/hutch` (multi-bot agent platform), a Go package |
| `garth`, `keelage`, `lockage`, `pilotage` | rejected — one live project each (PyPI, GitHub, Go) |
| `stallage` | clean — 9 characters, awkward to say |
| **`cranage`** | **clean** — 7 characters, pronounceable, one word, no software collision; "the charge for lifting cargo at a port" keeps the metaphor the rejected names shared (a fee paid to be admitted) |

## What changed in this commit

- Go module path: `keypod` → `cranage`, and every in-module import with it.
- `Makefile` ldflags target the new path. This is not cosmetic: with a stale path `-X` silently does
  nothing and the binary reports an empty version, which then breaks the release/`/healthz` check.
- This document and the ADR-0004 status.

## What deliberately did NOT change

Runtime identifiers stay `keypod` for now: the namespace `keypod-system`, the Deployment/Service names,
the image name, the binary (`cmd/keypod`, `bin/keypod`), the annotation keys `keypod.io/*`, the four
profile `ssh_host` values, and the board tenant.

Renaming those is a cutover with a window in which a running gateway must be replaced, and the board is
currently handing out instances to five agents. The rule from this project's own history applies:
**new → verify → migrate → retire**, with the old path left working until the new one is proven.

## The cutover, when it happens

1. New namespace `cranage-system`, new Deployment/Service names, annotations `cranage.io/*`, new image
   name — deployed **alongside** the running gateway, on a second port.
2. Point one non-`default` profile at it, verify a full session (identity, lease, `-L`, scp).
3. Move the remaining profiles one at a time; the old gateway keeps serving whoever is left.
4. Retire `keypod-system` and the old image tags; keep the old repository archived, not deleted.
5. Only then rename the board tenant and the annotation keys — those are the ones that need a data
   migration, because existing objects carry the old annotation.

Steps 1–2 are additive and reversible; step 4 is the only irreversible one.
