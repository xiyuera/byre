# Release assets carry the image digest — 2026-11-18

Closes the gap left by v0.3.1/v0.3.2: those two releases published binaries with no image digest, so
"which image belongs to this version" was answered from a card, not from the release page. Publishing
the digest has to be done by the pipeline, or the next release has the same hole.

## What was wrong

Two pipelines, one tag. `release.yml` built and uploaded the binaries; the image was built and pushed
separately (`scripts/publish-image.sh`, or by hand). That script appends `IMAGE_DIGEST=` to the local
`dist/build-info.txt` — a file that was already uploaded, and that is covered by `SHA256SUMS`, so
rewriting it afterwards breaks every checksum a reader already verified. The two releases were
therefore repaired by uploading a **new** asset (`images.txt`) by hand. Nothing produced it for the
next tag.

## What changed

| piece | change |
|---|---|
| `scripts/record-release-image.sh` (new) | asks the registry what `<tag>` resolves to (`HEAD /v2/<image>/manifests/<tag>`), fetches the manifest and hashes it, and writes `images.txt`. Exit 1 for a tag with no image; 2 for missing tooling or credentials; never invents a digest |
| `release.yml` | new step before publishing (`Record the image digest this tag points at`), and a new step after it (`The published images.txt is the registry's answer`) that downloads the asset back, compares it byte for byte with what the job wrote, and asks the registry once more |
| `scripts/assert-image-digest.sh` | now asserts the **registry prefix** as well as the digest, and exits 2 when it cannot qualify the check |
| `scripts/test_assert_states.sh` (new) | the five states of that assertion, run against a live cluster |
| `Makefile` (`image-verify`) | passes `EXPECT_REGISTRY`; `IMAGES=` takes the release's own `images.txt` |
| `docs/handbook/operations.md`, `docs/release.md` | the deploy path and the pipeline description, corrected: five assets, not four |

The order of operations became a rule: **push the image for the tag, then push the tag**. A tag whose
image is missing fails the record step. The alternative — publishing no digest, or a guessed one — is
what took a manual repair to fix twice.

## Evidence

### 1. The registry lookup, four states (local, against the real registry)

```
record: existing tag resolves to a digest                          rc=0
        digest sha256:9061566f…db78 (manifest body hash agrees)
record: tag with no image fails, writes no file                    rc=1  (HTTP 404, no images.txt)
record: bad credential fails                                       rc=1  (HTTP 401 → needs read:package)
record: missing credentials is a usage error                       rc=2  (not 1: not a checked failure)
```

### 2. The running-image assertion, five states (live cluster, the pod that serves traffic)

```
MATCH         release images.txt names the running digest+registry     rc=0
CANNOT CHECK  bare digest, no EXPECT_REGISTRY                          rc=2   ← not a pass
MATCH         bare digest + the right EXPECT_REGISTRY                  rc=0
MISMATCH      same digest, wrong registry in imageID                   rc=1   ← a digest-only check passes here
MISMATCH      right registry, older digest                             rc=1
```

The fourth row is the reason the prefix is asserted: with identical blobs under two registry names,
containerd downloads nothing and `imageID` keeps naming the previous repository.

`make image-verify IMAGES=<images.txt from the release>` → `PASS`, `rc=0`, against the running pod.

### 3. The same two steps inside CI (branch dry run, before any tag was cut)

A temporary workflow on the branch ran the delivered commands in the job image (`alpine`, no docker, no
python), with the per-job token:

```
$ curl -u "${GITHUB_ACTOR}:${GITHUB_TOKEN}" https://<registry>/v2/<image>/tags/list
{"name":"…","tags":["v0.2.0","v0.3.0","v0.3.1","v0.3.2"]}
$ scripts/record-release-image.sh <registry>/<image> v0.3.2 /tmp/pos      → rc=0
$ scripts/record-release-image.sh <registry>/<image> v9.9.9-nope /tmp/neg → rc=1, no file written
release asset: sha256:9061566f…db78
registry now : sha256:9061566f…db78
agree
```

Two things this measured that the local run could not:

- the registry accepts **Basic** auth (`actor:token`) and rejects `Authorization: token …` with 401,
  although the API side accepts the header — so the workflow uses Basic;
- the scripts were committed **without the executable bit**, and the step failed with `exit 126
  (Permission denied)`. Locally `bash scripts/…` hides that. Fixed (mode 100755) and re-run green.

### 4. What CI cannot assert, and where that check lives instead

The job pod runs with `automountServiceAccountToken: false`, no `kubeconfig`, and no `kubectl` in the
image (`kubectl: none`, `serviceaccount token: absent`). A release job therefore **cannot** read a pod's
`imageID`. Giving it cluster credentials would grant read access to anyone who can push a branch.

So the two halves are split, deliberately:

- **release ↔ registry** is asserted in CI, on every tag (step 4 above);
- **release ↔ running pod** is one command for the deployer:
  `make image-verify IMAGES=images.txt` (download the asset first), three states, `2` when it cannot
  tell. See `docs/handbook/operations.md`.

## Not covered here

The v0.3.3 tag is the end-to-end proof that the release page now carries the digest without a hand
edit. It is cut after this change is on `main`, because a tag must point at a commit that reached
`main` — the pipeline change cannot be proven by a tag that predates it.
