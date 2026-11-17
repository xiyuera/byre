#!/usr/bin/env bash
# Publish a container image and record the digest the registry assigned to it.
#
# A tag is a mutable pointer. Push the same tag twice and the node keeps its cached layers
# (`imagePullPolicy: IfNotPresent`), so the Deployment says `v0.2.0` while the process is still the
# previous build — it reports the tag it was asked for, not the content it runs. The digest is the only
# reference that names what was published, so the push and the record of the push have to be the same
# step: otherwise the deployment has nothing to pin to.
#
#   scripts/publish-image.sh <registry>/<image>:<tag> [out-dir]
#
# Writes into <out-dir> (default `dist/`):
#   images.txt       IMAGE_REF=<registry>/<image>@sha256:…   the reference a Deployment should carry
#                    IMAGE_TAG=…, IMAGE_DIGEST=sha256:…, PUSHED_AT=…
#   build-info.txt   one appended IMAGE_DIGEST= line, when that release asset already exists
#
# Container runtime: $CONTAINER_RUNTIME if set, else the first of docker / podman on PATH.
#
# Exit codes: 0 pushed and recorded; 1 the push failed or produced no digest; 2 usage or tooling.
set -euo pipefail

if [ $# -lt 1 ] || [ $# -gt 2 ]; then
  echo "usage: $0 <registry>/<image>:<tag> [out-dir]" >&2
  exit 2
fi

ref=$1
out=${2:-dist}
case "$ref" in
  */*:*) ;;
  *) echo "ref must carry a tag or digest: $ref" >&2; exit 2 ;;
esac
if [ "${ref##*@}" != "$ref" ]; then
  echo "ref is already a digest; nothing to publish: $ref" >&2
  exit 2
fi

runtime=${CONTAINER_RUNTIME:-}
if [ -z "$runtime" ]; then
  for candidate in docker podman; do
    if command -v "$candidate" >/dev/null 2>&1; then runtime=$candidate; break; fi
  done
fi
[ -n "$runtime" ] || { echo "no container runtime found (docker/podman, or set CONTAINER_RUNTIME)" >&2; exit 2; }

mkdir -p "$out"
log=$(mktemp)
trap 'rm -f "$log"' EXIT

# Push first; a tag in the output is not evidence, the digest is.
if ! "$runtime" push "$ref" >"$log" 2>&1; then
  sed 's/^/  /' "$log" >&2 || true
  echo "push failed: $ref" >&2
  exit 1
fi

digest=$(grep -oE 'digest: sha256:[0-9a-f]{64}' "$log" | head -1 | cut -d' ' -f2 || true)
if [ -z "$digest" ]; then
  # Some runtime versions print progress only. The local image record still knows what the registry
  # returned for this repository, which is the same value the node resolves.
  repo_digest=$("$runtime" image inspect --format '{{index .RepoDigests 0}}' "$ref" 2>/dev/null || true)
  case "$repo_digest" in
    *@sha256:*) digest=${repo_digest##*@} ;;
  esac
fi
if [ -z "$digest" ]; then
  echo "push reported success but no digest was found (registry returned none, or the image was not" >&2
  echo "tagged for this repository): $ref" >&2
  exit 1
fi

# <registry>/<image>:<tag> -> <registry>/<image>
bare=${ref%:*}
if [ "${bare##*/}" != "${bare##*:}" ]; then bare=${ref%:*} ; fi
image_ref="${bare}@${digest}"

{
  echo "# written by scripts/publish-image.sh — the digest, not the tag, is what a Deployment pins."
  echo "IMAGE_REF=${image_ref}"
  echo "IMAGE_TAG=${ref}"
  echo "IMAGE_DIGEST=${digest}"
  echo "PUSHED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
} > "$out/images.txt"

if [ -f "$out/build-info.txt" ] && ! grep -q '^IMAGE_DIGEST=' "$out/build-info.txt"; then
  printf 'IMAGE_DIGEST=%s\n' "$digest" >> "$out/build-info.txt"
fi

echo "pushed   $ref"
echo "digest   $digest"
echo "recorded $out/images.txt"
echo
echo "deploy it by digest:"
echo "  kubectl -n byre-system set image deploy/byre byre=$image_ref"
