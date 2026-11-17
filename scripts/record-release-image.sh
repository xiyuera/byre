#!/usr/bin/env bash
# Record the digest that a tag points at *in the registry*, as a release asset.
#
# The image build and the release build are two pipelines: `make dist` produces the binaries and
# `scripts/publish-image.sh` pushes an image, possibly minutes apart and on different machines. The
# release therefore cannot read the digest off its own disk — the only place that knows what a tag
# currently resolves to is the registry. This asks the registry.
#
#   scripts/record-release-image.sh <registry-image> <tag> [out-dir]
#   e.g. REGISTRY_USER=ci REGISTRY_TOKEN=… \
#        scripts/record-release-image.sh git.example.com:8443/byre/byre v0.3.3 dist
#
# Writes <out-dir>/images.txt with the same fields `scripts/publish-image.sh` writes:
#   IMAGE_REF=<registry>/<image>@sha256:…   the reference a Deployment should pin
#   IMAGE_TAG=<registry>/<image>:<tag>      the tag this was resolved from, for the reader
#   IMAGE_DIGEST=sha256:…                   what the registry returned
#   PUSHED_AT=…                             `unknown` when the registry does not say (see below)
#
# Exit codes: 0 recorded; 1 the tag does not resolve to a digest (never invents one); 2 usage or a
# missing tool. A missing tag is an error on purpose: a release with no digest reads exactly like a
# release whose digest matched, and the point of the asset is to make that impossible.
set -euo pipefail

if [ $# -lt 2 ] || [ $# -gt 3 ]; then
  echo "usage: $0 <registry-image> <tag> [out-dir]" >&2
  exit 2
fi

image=$1
tag=$2
out=${3:-dist}

case "$image" in
  */*) ;;
  *) echo "give <registry-host>/<image>: $image" >&2; exit 2 ;;
esac
case "$image" in
  *@sha256:*) echo "give the tagless image name, not a reference: $image" >&2; exit 2 ;;
esac
case "$tag" in
  */*) echo "tag must be a tag, not a path: $tag" >&2; exit 2 ;;
esac

command -v curl >/dev/null 2>&1 || { echo "curl not on PATH" >&2; exit 2; }

host=${image%%/*}
path=${image#*/}
if [ -z "${REGISTRY_USER:-}" ] || [ -z "${REGISTRY_TOKEN:-}" ]; then
  echo "set REGISTRY_USER and REGISTRY_TOKEN (a token with read:package) to read ${host}" >&2
  exit 2
fi

# The credential goes into a 0600 curl config, not into argv: `-u user:token` is visible in `ps` to
# every process in the same pod, and a release job is not a special case.
cfg=$(mktemp)
trap 'rm -f "$cfg"' EXIT
chmod 600 "$cfg"
printf 'user = "%s:%s"\n' "$REGISTRY_USER" "$REGISTRY_TOKEN" > "$cfg"

accept='application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json'
url="https://${host}/v2/${path}/manifests/${tag}"

hdrs=$(mktemp)
trap 'rm -f "$cfg" "$hdrs"' EXIT
code=$(curl -sS -K "$cfg" -H "Accept: ${accept}" -I -o /dev/null -D "$hdrs" -w '%{http_code}' "$url" || true)
case "$code" in
  200) ;;
  401|403)
    echo "registry refused the credential for ${image} (HTTP ${code}): it needs read:package" >&2
    exit 1 ;;
  404)
    echo "no manifest for ${image}:${tag} in the registry (HTTP 404)." >&2
    echo "Push the image for this tag before tagging the release; a digest cannot be recorded for" >&2
    echo "an image that was never pushed, and guessing one would publish a number nobody can verify." >&2
    exit 1 ;;
  *)
    echo "registry lookup for ${image}:${tag} returned HTTP ${code}" >&2
    exit 1 ;;
esac

digest=$(sed -n 's/^[Dd]ocker-[Cc]ontent-[Dd]igest: *//p' "$hdrs" | tr -d '\r' | head -1)
case "$digest" in
  sha256:*) ;;
  *) echo "registry answered 200 for ${image}:${tag} but sent no Docker-Content-Digest header" >&2
     exit 1 ;;
esac

# A header is a claim about a body. Fetch the body and hash it: if the two disagree, one of them is
# wrong and neither is a safe thing to pin. This is also what makes "we never invent a digest" true
# rather than aspirational.
body=$(mktemp)
trap 'rm -f "$cfg" "$hdrs" "$body"' EXIT
code=$(curl -sS -K "$cfg" -H "Accept: ${accept}" -o "$body" -w '%{http_code}' "$url" || true)
[ "$code" = 200 ] || { echo "manifest body fetch returned HTTP ${code} for ${image}:${tag}" >&2; exit 1; }
body_digest="sha256:$(sha256sum "$body" | cut -d' ' -f1)"
if [ "$body_digest" != "$digest" ]; then
  echo "the registry header says ${digest} but the manifest it just served hashes to ${body_digest}" >&2
  exit 1
fi
rm -f "$body"

# A registry manifest carries no push timestamp, and the mtime of the object on the server is not the
# push either. `unknown` is the honest value; a fabricated date would be the field everybody later
# treats as authoritative.
pushed_at=unknown

mkdir -p "$out"
{
  echo "# written by scripts/record-release-image.sh — the digest, not the tag, is what a Deployment pins."
  echo "# IMAGE_DIGEST is what the registry answered for ${image}:${tag} at release time; the manifest"
  echo "# body was hashed and compared with that header before this file was written."
  echo "# PUSHED_AT is unknown: a registry manifest has no push timestamp, and inventing one would make"
  echo "# this file look like it records when the image was pushed. It does not."
  echo "IMAGE_REF=${image}@${digest}"
  echo "IMAGE_TAG=${image}:${tag}"
  echo "IMAGE_DIGEST=${digest}"
  echo "PUSHED_AT=${pushed_at}"
} > "${out}/images.txt"

echo "registry  ${image}:${tag}"
echo "digest    ${digest} (manifest body hash agrees)"
echo "recorded  ${out}/images.txt"
echo
echo "deploy it by digest:"
echo "  kubectl -n byre-system set image deploy/byre byre=${image}@${digest}"
