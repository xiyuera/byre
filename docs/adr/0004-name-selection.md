# ADR-0004 — Choosing the name

- Status: accepted
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
