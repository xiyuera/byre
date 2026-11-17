#!/usr/bin/env bash
# Assert the running pod is the image that was published — compare digests AND registry, not tags.
#
# Two silent failures this catches:
#
#   1. A Deployment referencing `byre:v0.2.0` with `imagePullPolicy: IfNotPresent` after that tag was
#      re-pushed reports the new tag and runs the old layers.
#   2. A Deployment that *was* switched to a digest from another registry, where the same blobs are
#      already on the node: containerd downloads nothing and `imageID` keeps naming the previous
#      repository. The digest matches, the registry does not, and the pod is not running what the
#      release page says it is. A digest-only check passes here — which is why the registry prefix is
#      asserted too (measured on t_2aacdbdb).
#
#   scripts/assert-image-digest.sh [images.txt | sha256:… | <image>@sha256:…] [namespace] [deployment]
#
# The first argument may be a digest (`sha256:…` / `<image>@sha256:…`), or the path to images.txt as
# written by `scripts/publish-image.sh` or `scripts/record-release-image.sh` — the one attached to the
# release. Default file: dist/images.txt.
#
# When the source names only a digest, the expected registry has to come from somewhere: pass
# `EXPECT_REGISTRY=<host>[:port]`. Without it the run exits 2 rather than reporting a digest match it
# could not qualify — "could not check" is not "checked and fine".
#
# Exit codes: 0 digest and registry both match; 1 mismatch or no running pod; 2 usage or tooling
# (never "pass").
set -euo pipefail

want_source=${1:-dist/images.txt}
namespace=${2:-byre-system}
deploy=${3:-byre}

command -v kubectl >/dev/null 2>&1 || { echo "kubectl not on PATH" >&2; exit 2; }

want_host=''
case "$want_source" in
  sha256:*) want=$want_source ; want_host=${EXPECT_REGISTRY:-} ;;
  *@sha256:*) want=${want_source##*@} ; want_host=${want_source%%/*} ;;
  *)
    [ -r "$want_source" ] || { echo "cannot read $want_source (run: make image-push, or download the release's images.txt)" >&2; exit 2; }
    want=$(sed -n 's/^IMAGE_DIGEST=//p' "$want_source" | head -1)
    # IMAGE_REF is the line that carries the registry; IMAGE_TAG is a fallback for files written
    # before IMAGE_REF existed.
    ref=$(sed -n 's/^IMAGE_REF=//p' "$want_source" | head -1)
    [ -n "$ref" ] || ref=$(sed -n 's/^IMAGE_TAG=//p' "$want_source" | head -1)
    [ -z "$ref" ] || want_host=${ref%%/*}
    ;;
esac
case "$want" in
  sha256:*) ;;
  *) echo "no digest in $want_source" >&2; exit 2 ;;
esac
if [ -z "$want_host" ]; then
  echo "$want_source names a digest but not the registry it must come from." >&2
  echo "Pass EXPECT_REGISTRY=<host>[:port], or use the release's images.txt (it carries IMAGE_REF)." >&2
  exit 2
fi
want_host=${want_host#*://}

# Label selector from the Deployment itself, so a renamed label cannot make the check look at the
# wrong pods (an empty selector would match every pod in the namespace).
labels=$(kubectl -n "$namespace" get deploy "$deploy" -o go-template='{{range $k,$v := .spec.selector.matchLabels}}{{$k}}={{$v}},{{end}}' 2>/dev/null || true)
labels=${labels%,}
[ -n "$labels" ] || { echo "deployment $namespace/$deploy not found, or it has no matchLabels" >&2; exit 1; }

# Only a pod that is Running and not terminating counts: a terminating pod still carries the old
# imageID for a while, and comparing against it would report the previous build as the live one.
image_ids=$(kubectl -n "$namespace" get pods -l "$labels" -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{if eq .status.phase "Running"}}{{range .status.containerStatuses}}{{.name}}={{.imageID}}{{"\n"}}{{end}}{{end}}{{end}}{{end}}' 2>/dev/null || true)
[ -n "$image_ids" ] || { echo "no running pod for $namespace/$deploy" >&2; exit 1; }

fail=0
while IFS= read -r line; do
  [ -n "$line" ] || continue
  name=${line%%=*}
  image_id=${line#*=}
  got=${image_id##*@}
  got_host=${image_id%%/*}
  got_host=${got_host#*://}
  if [ "$got" != "$want" ]; then
    printf 'FAIL  %s  running=%s expected=%s\n' "$name" "${got:-<none>}" "$want"
    fail=1
    continue
  fi
  if [ "$got_host" != "$want_host" ]; then
    printf 'FAIL  %s  digest matches but the registry does not: running=%s expected=%s\n' \
      "$name" "$got_host" "$want_host"
    printf '      the node already had these blobs under the other name, so containerd pulled nothing\n'
    printf '      and imageID still reports it; the pod is not running what the release page names.\n'
    fail=1
    continue
  fi
  printf 'PASS  %s  %s@%s\n' "$name" "$got_host" "$got"
done <<EOF
$image_ids
EOF

[ $fail -eq 0 ] || { echo "the running image is not the published one" >&2; exit 1; }
