#!/usr/bin/env bash
# Exercise every state of scripts/assert-image-digest.sh against the live cluster, and record the
# exit codes. The states matter more than the happy path: a check that cannot see a wrong registry
# is the bug this script exists to prevent, so MISMATCH and CANNOT-CHECK must be demonstrated too.
#
#   scripts/test_assert_states.sh [images.txt-downloaded-from-a-release]
#
# Not wired into CI: it reads the cluster. Run it from wherever `kubectl` points at the cluster.
set -uo pipefail

repo_dir=$(cd "$(dirname "$0")/.." && pwd)
assert="${repo_dir}/scripts/assert-image-digest.sh"
rel_txt=${1:?pass the images.txt downloaded from a release, e.g. evidence/assert/images-v032.txt}
digest=$(sed -n 's/^IMAGE_DIGEST=//p' "$rel_txt" | head -1)
host=$(sed -n 's/^IMAGE_REF=//p' "$rel_txt" | head -1)
host=${host%%/*}
[ -n "$digest" ] && [ -n "$host" ] || { echo "cannot read IMAGE_DIGEST/IMAGE_REF from $rel_txt" >&2; exit 2; }

# A digest from an older release: same registry, different content, so only the digest differs.
# v0.3.1, read from the registry (HEAD /v2/byre/byre/manifests/v0.3.1).
old_digest=${OLD_DIGEST:-sha256:f3c3239d406f7c033e0eb6e67694c0f0e84a8e2caef206333a3dfe44be49c65c}
# A registry name that cannot match the running pod: this is how the registry assertion is shown to
# fail rather than pass by accident. Override when a real second registry exists to point at.
wrong_registry=${WRONG_REGISTRY:-example.invalid:5001}

run() {
  label=$1; shift
  echo "### ${label}"
  out=$("$@" 2>&1); rc=$?
  printf '%s\n' "$out" | sed 's/^/    /'
  echo "    rc=${rc}"
  echo
}

echo "source: $rel_txt"
echo "digest: $digest"
echo "host:   $host"
echo

run "MATCH — release images.txt names the running digest and registry (rc 0)" \
  env -u EXPECT_REGISTRY "$assert" "$rel_txt"

run "CANNOT CHECK — bare digest, no EXPECT_REGISTRY (rc 2, must not read as pass)" \
  env -u EXPECT_REGISTRY "$assert" "$digest"

run "MATCH — bare digest plus the right EXPECT_REGISTRY (rc 0)" \
  env EXPECT_REGISTRY="$host" "$assert" "$digest"

run "MISMATCH — same digest, wrong registry in imageID (rc 1; this is what a digest-only check misses)" \
  env EXPECT_REGISTRY="$wrong_registry" "$assert" "$digest"

run "MISMATCH — right registry, older digest (rc 1)" \
  env EXPECT_REGISTRY="$host" "$assert" "$old_digest"
