## What changed

<!-- one paragraph; the "why" belongs in the commit body -->

## Invariants touched

- [ ] lease lifetime (`ttl` / `grace` / renewal)
- [ ] identity resolution
- [ ] adoption / orphan handling
- [ ] backend driver interface
- [ ] none of the above (docs, tests, refactor)

## Verification

```sh
make lint test
go test -race ./...
```

- Real cluster used (backend type, pool size):
- Checks run (AC numbers from `docs/acceptance/`):

## Checklist

- [ ] no environment specifics (hosts, namespaces, image tags, key paths, identity names)
- [ ] no new runtime dependency (stdlib + x/crypto + k8s client only)
- [ ] a test would fail if this change were reverted
