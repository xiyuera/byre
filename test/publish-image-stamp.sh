#!/usr/bin/env bash
# Refuse to publish an image whose binary cannot say what it is.
#
# v0.4.0 shipped as `byre_build_info{version="dev",commit="none"}` while every pipeline step reported
# success: the image had been built without --build-arg. The only artifact that records the difference is
# the running process, so the check has to ask it — this test drives scripts/publish-image.sh through a
# stub runtime that answers --version, and asserts both halves: the refusal, and a normal publish.
#
#   test/publish-image-stamp.sh        (exit 0 = both cases behave)
set -euo pipefail

here=$(cd "$(dirname "$0")/.." && pwd)
script="$here/scripts/publish-image.sh"
[ -f "$script" ] || { echo "missing $script" >&2; exit 2; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Stub runtime: /usr/bin/env sh so it runs anywhere the script runs.
make_runtime() {
  dir=$1
  version=$2
  mkdir -p "$dir"
  cat >"$dir/docker" <<EOF
#!/bin/sh
case "\$1" in
  run)     printf '%s\n' "$version" ;;
  push)    echo "The push refers to repository [example/byre]"; echo "digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" ;;
  image)   echo "example/byre@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" ;;
  *)       echo "unexpected runtime call: \$*" >&2; exit 1 ;;
esac
EOF
  chmod +x "$dir/docker"
}

fail=0

# 1. The unstamped default must stop the publish before anything is pushed.
make_runtime "$work/unstamped" "dev (none, unknown)"
mkdir -p "$work/out-bad"
if PATH="$work/unstamped:$PATH" CONTAINER_RUNTIME=docker "$script" example/byre:v0.4.0 "$work/out-bad" \
    >"$work/bad.log" 2>&1; then
  echo "FAIL: publishing an image that reports 'dev (none, unknown)' was allowed" >&2
  fail=1
else
  rc=$?
  [ "$rc" -eq 2 ] || { echo "FAIL: expected exit 2 for an unstamped image, got $rc" >&2; fail=1; }
  grep -q 'built without --build-arg' "$work/bad.log" ||
    { echo "FAIL: the refusal did not say why" >&2; fail=1; }
  [ ! -s "$work/out-bad/images.txt" ] ||
    { echo "FAIL: a refused publish still recorded images.txt" >&2; fail=1; }
fi

# 2. A stamped image publishes, and the digest is recorded.
make_runtime "$work/stamped" "v0.4.1 (abc1234, 2026-11-20T06:00:00Z)"
mkdir -p "$work/out-ok"
if PATH="$work/stamped:$PATH" CONTAINER_RUNTIME=docker "$script" example/byre:v0.4.1 "$work/out-ok" \
    >"$work/ok.log" 2>&1; then
  grep -q '^IMAGE_DIGEST=sha256:0123456789abcdef' "$work/out-ok/images.txt" ||
    { echo "FAIL: publish did not record the digest" >&2; cat "$work/ok.log" >&2; fail=1; }
  grep -q 'image reports: v0.4.1' "$work/ok.log" ||
    { echo "FAIL: publish did not echo the stamp it verified" >&2; fail=1; }
else
  echo "FAIL: a stamped image was not published" >&2
  cat "$work/ok.log" >&2
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  echo "publish-image-stamp: both cases ok"
fi
exit "$fail"
