#!/usr/bin/env bash
# Assert the running pod is the image that was published — compare digests, not tags.
#
# The failure this catches is silent: a Deployment referencing `byre:v0.2.0` with
# `imagePullPolicy: IfNotPresent` after that tag was re-pushed reports the new tag and runs the old
# layers. `/healthz` prints the build stamped into the binary, but nobody reads it on every rollout, so
# the check has to be a command that exits non-zero.
#
#   scripts/assert-image-digest.sh [images.txt | sha256:…] [namespace] [deployment]
#
# The first argument may be a digest (`sha256:…` / `<image>@sha256:…`), or the path to images.txt as
# written by scripts/publish-image.sh. Default file: dist/images.txt.
#
# Exit codes: 0 digest matches; 1 mismatch or no running pod; 2 usage or tooling (never "pass").
set -euo pipefail

want_source=${1:-dist/images.txt}
namespace=${2:-byre-system}
deploy=${3:-byre}

command -v kubectl >/dev/null 2>&1 || { echo "kubectl not on PATH" >&2; exit 2; }

case "$want_source" in
  sha256:*) want=$want_source ;;
  *@sha256:*) want=${want_source##*@} ;;
  *)
    [ -r "$want_source" ] || { echo "cannot read $want_source (run: make image-push)" >&2; exit 2; }
    want=$(sed -n 's/^IMAGE_DIGEST=//p' "$want_source" | head -1)
    ;;
esac
case "$want" in
  sha256:*) ;;
  *) echo "no digest in $want_source" >&2; exit 2 ;;
esac

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
  if [ "$got" = "$want" ]; then
    printf 'PASS  %s  %s\n' "$name" "$got"
  else
    printf 'FAIL  %s  running=%s expected=%s\n' "$name" "${got:-<none>}" "$want"
    fail=1
  fi
done <<EOF
$image_ids
EOF

[ $fail -eq 0 ] || { echo "the running image is not the published one" >&2; exit 1; }
