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

## Before opening a PR

```sh
make fmt vet test
make lint          # uses golangci-lint when installed
go test -race ./...
```

## Adding a backend driver

1. Implement `internal/backend.Backend` (`Acquire`, `Release`, `List`, `Adopt`).
2. Extend `internal/backend/config.go` validation with the new `type`/`provider.kind` and its required
   fields — unknown values must keep failing.
3. Add unit tests with a fake API client (no live cluster in unit tests).
4. Add an `examples/<driver>/` directory with a minimal, placeholder-filled manifest.
5. Document it in `docs/architecture.md` §5 and, if it needs new tunables, in `docs/operations.md`.

## Changing the lease semantics

Anything touching `ttl`, `grace`, renewal or adoption needs an ADR (see `docs/adr/`) and a test that
fails on the previous behaviour. Two behaviours that must never regress:

- a reconnect within `grace` returns the *same* instance;
- an expired lease never hands its instance to a different identity.

## Commit messages

`<area>: <imperative summary>` — e.g. `lease: renew on channel open, not only on data`.
Body: why, then what changed, then how it was verified. Reference the ADR when one applies.

## Reporting a bug

Include: keypod version (`keypod -version`), the relevant log lines (identity prefixes are fine,
full keys never are), the backend type, and whether the client uses `ControlMaster`.

## Security

Do not open public issues for a vulnerability. Send a description and reproduction steps to the
maintainers' private address listed in `SECURITY.md` once it exists (M5); until then, contact a
maintainer directly.
