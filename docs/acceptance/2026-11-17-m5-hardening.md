# Hardening record — 2026-11-17 (M5)

Follow-up to `2026-11-17-m4.md`: the parts of the charter that were still unverified after the
cluster run.

## 1. SFTP / subsystem pass-through (must-have #5)

`sftp` is not a separate protocol on the wire — it is a session channel with a `subsystem` request.
The gateway forwards session requests verbatim in both directions, so the test asserts the request
reaches the instance and the reply comes back:

```
$ go test ./internal/sshfront -run TestSubsystemPassThrough -v
=== RUN   TestSubsystemPassThrough
--- PASS: TestSubsystemPassThrough (0.03s)
```

The fake instance answers `subsystem:sftp`, which is what an sftp server's first reply looks like from
the gateway's point of view. `scp` uses the `exec` path and is covered by the exec test.

## 2. Failure paths

| scenario | expected | test |
|---|---|---|
| backend cannot provide an instance | authentication succeeds (key is known), session is refused, **no lease is left behind** | `TestBackendFailureRefusesSession` |
| instance refuses connections after checkout | client is disconnected, lease drops back to refs=0 so the reaper can reclaim it (a stuck ref would pin a dead instance forever) | `TestDialFailureReleasesRef` |
| claim never becomes dialable | claim is deleted by the driver before returning the error | `TestClaimIsNotReadyUntilEndpointResolves` |
| pod never becomes Ready | pod is deleted; failure surfaces after `ready_timeout` | `TestPodWaitsForReady` |
| stale pool objects after a crash | destroyed during adoption, then the pool refills | `TestAdoptRebuildsIdentityTable` |

## 3. Deployment manifests validated against the API server

`kubectl apply -k` was not available in the build environment (no container runtime, no kubectl), so
each rendered object was sent to the cluster with `?dryRun=All` — the same schema/validation path
`apply` uses, without persisting anything:

```
Namespace        OK
ServiceAccount   OK
Role             OK
RoleBinding      OK
Deployment       OK
Service          OK
ConfigMap        OK      (generated from config.example.yaml, as kustomize does)
```

The temporary namespace needed for those checks (`dry-run` still requires the parent namespace to
exist) was deleted afterwards; `GET /api/v1/namespaces/byre-system` returns `NotFound`.

**Not verified:** actually running the image inside the cluster. That needs an image build
(`docker build` / `ko` / `buildah`) plus `kubectl apply -k`, none of which exist in the build
container. The gateway binary itself was exercised in-cluster from the shared volume during the M4
run, so the runtime behaviour is covered; the *image* + *manifest apply* path is the only remaining
unproven step, and it is a packaging concern rather than a protocol one.

## 4. Test inventory

41 test functions across four packages; `go test -race ./...` green.
