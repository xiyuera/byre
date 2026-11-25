# Decision records

Each record states the decision, the conditions under which it would be reversed, and what it cost.
Read them in order if you are changing the lease model; read the one named in a code comment if you
are changing that piece.

| record | decision |
|---|---|
| [0001-identity-is-the-public-key.md](0001-identity-is-the-public-key.md) | ADR-0001 — Identity is the client's public key |
| [0002-lease-per-identity.md](0002-lease-per-identity.md) | ADR-0002 — Lease per identity, not per connection |
| [0003-declarative-backends.md](0003-declarative-backends.md) | ADR-0003 — Backends are declared in configuration, not compiled in |
| [0004-name-selection.md](0004-name-selection.md) | ADR-0004 — Choosing the name |
| [0005-readiness-is-dialable.md](0005-readiness-is-dialable.md) | ADR-0005 — Readiness means "one advertised address answers", not "a status field is present" |
| [0006-host-key-ownership.md](0006-host-key-ownership.md) | ADR-0006 — The product owns host key persistence |
| [0007-provisioning-is-a-vocabulary.md](0007-provisioning-is-a-vocabulary.md) | ADR-0007 — Provisioning is a vocabulary, not a product |
| [0008-lease-liveness.md](0008-lease-liveness.md) | ADR-0008 — A lease is invalid the moment its instance is gone |
| [0009-command-backend-list-semantics.md](0009-command-backend-list-semantics.md) | ADR-0009 — `list` exists to adopt; "no annotation" means "unknown owner", not "stale" |
| [0010-multi-replica-preconditions.md](0010-multi-replica-preconditions.md) | ADR-0010 — A second replica needs leader election or CAS; instances are not owned by the Deployment |
| [0011-gateway-readiness.md](0011-gateway-readiness.md) | ADR-0011 — Gateway readiness is "this process can provision", not "the process is alive" |
| [0012-lock-granularity-and-phases.md](0012-lock-granularity-and-phases.md) | ADR-0012 — The lease table is locked briefly, and a row says which phase it is in |
| [0013-lease-state-in-a-crd.md](0013-lease-state-in-a-crd.md) | ADR-0013 — Lease state belongs in a CRD; memory holds only what is derived |
| [0014-listing-must-not-hide-objects.md](0014-listing-must-not-hide-objects.md) | ADR-0014 — `list` reports every object it selected, including the ones it cannot dial |
