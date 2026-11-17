# ADR-0004 — Choosing the name

- Status: **superseded** — the name became `cranage`; see `docs/rename-to-cranage.md`
  (the correction below is kept for its reasoning, not because the name survived it)
- Date: 2026-11-17
- Deciders: maintainers

## Criteria

Taken from the Apache Software Foundation naming process and general practice for Go projects:

1. short and memorable — one word, ≤ 8 characters, unambiguous pronunciation;
2. descriptive of the domain, or a memorable metaphor, but **not** a generic noun that cannot be searched;
3. no collision with an existing project in the same domain, no trademark risk, no confusion with a
   well-known product;
4. no structural prefix (`ssh-`, `sandbox-`) — the prefix describes the implementation, not the idea,
   and those names are unfindable;
5. usable as a repository name, binary name, module path and image name without modification.

## Candidates and collision checks

Checked against GitHub-wide search, `pkg.go.dev`, npm and the project's own registry.

| candidate | meaning | result |
|---|---|---|
| `berth` | a ship's slot in a port | rejected: 4+ Go projects and an unrelated AI workflow tool use the name |
| `roost` | a bird's resting place | rejected: 3+ Go packages |
| `moorage` | the fee/place for mooring | rejected: a Go deployment tool and an npm package |
| `quay` | a dock | rejected: a well-known container registry |
| `stallage` | the right to keep a market stall; the fee for it | free, but 9 characters and obscure in speech |
| `hutch` | a small cage | free in this domain, weak semantic link to identities |
| **`keypod`** | key (identity) + pod (a small self-contained instance) | **selected** |

## Decision

The project is named **keypod**.

It satisfies every criterion: 6 characters, one word, pronounceable in any language, no prefix, and no
same-domain collision. The metaphor states the two core concepts of the design — the key is the identity
(ADR-0001) and the pod is the disposable instance the lease hands out (ADR-0002).

Two caveats, accepted deliberately:

- "pod" is Kubernetes-flavoured, while the project is deliberately backend-agnostic (ADR-0003). The
  README defines the term as "a small self-contained instance", and the `static` backend shows the
  gateway working with instances that are not pods at all. A container-orchestrator association in the
  name is acceptable because the majority deployment is Kubernetes-based.
- Names can be exhausted later (a search-engine collision, a product launch). The cost of renaming at
  v0 is one `sed` and one repository rename, so the name is not treated as irreversible.

## Module path

The Go module is `keypod` (a bare path, not a domain). It builds and tests offline, carries no
organisational domain, and can be rewritten to a canonical hosted path if the project is ever published.

## Notes for the record

`keypod` also appeared as `key_pods` — the name of a single endpoint in an unrelated SDK — which is not
a project name and does not create ambiguity.

## Correction — the collision check for `keypod` was not done to the same standard

Written the same day, after an independent legal review found what this ADR had claimed did not exist.
This section is kept rather than edited so the mistake stays legible.

**What this ADR got wrong.** The table above rejects four candidates only after checking each against
several registries, then marks `keypod` "selected" on the strength of a single shallow search. That
asymmetry — seven filters for the alternatives, one search for the winner — is how a false negative
happens. `keypod` is **not** free:

| prior use | when | domain | note |
|---|---|---|---|
| `anusii/keypod` | 2023– | key-value store, GPL-3.0 | closest in meaning: "a key and a pod/box" for developers |
| `twixxiye/keypod` | 2025– | Android music player, GPL-3.0, has releases | same name, unrelated function |
| `isso-719/keypod` (`keypod.net`) | 2020 | Ruby web app | same name |
| `stevepham/keypod` | 2020 | Kotlin library, published on JitPack | the name already occupies a package coordinate |
| `KEYPOD` key safe (Extremehorizon/Northcore) | 2005– | physical key storage | different class; shows the word is a product name, not a coined term |
| `KeyPod` smart key box (QIUI) | — | consumer hardware + app | a Chinese entity ships a product under this exact name |
| US trademark `KEYPOD` 87404781 | 2017, abandoned 2017-11-16 | classes 009/042 | not a registration bar, but the mark has been claimed in this space before |

**Consequences of the error.** Apache-2.0 §6 grants no trademark rights, so the licence never protected
the name; a reader who finds four unrelated `keypod` projects reasonably concludes ours is a fork of one
of them. The cost is confusion and searchability rather than litigation, and it grows with every published
artifact (module path, image name, docs, blog posts) — which is why the decision belongs now, not later.

**Decision: unchanged for now, therefore provisional.** Renaming touches the repository path, the image
name, `docs/`, and the board tenant; doing it silently would be worse than documenting the risk. The name
stays `keypod` until the maintainer decides between:

1. **Rename** (`stallage`, `hutch`, or a new coinage) — cheapest today, and the only option that removes
   the collision. Any candidate needs the same depth of check that the alternatives above received: GitHub
   wide, `pkg.go.dev`, npm, JitPack, PyPI, Docker Hub, plus CNIPA/EUIPO/UKIPO for the intended class.
2. **Keep the name, publish the disclaimer** — README states no affiliation with any same-named product or
   trademark holder, no third-party logos, no `®`. Residual risk accepted in writing.
3. **Keep and defer** — recorded here, re-evaluated before the first public release. This is the state
   today, and it is a decision to carry risk, not to have resolved it.

**Rule extracted.** A naming ADR must state, per candidate, *what was searched and how deep*, and it must
apply the same depth to the winner as to the rejects. "No results found" from one query is not a collision
check; it is a search that has not finished.
