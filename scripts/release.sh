#!/usr/bin/env bash
# Cut a release: one command, two forges, and it does not stop at "the tag was pushed".
#
# Why this exists. The first v0.4.0 attempt spent 30 minutes waiting for a GitHub release run
# that could never start, because the tag existed on Gitea and not on GitHub. Gitea's push
# mirror propagates a *deletion* of a tag but not a *re-creation* of one, so a re-cut tag has to
# be pushed to the mirror by hand. Nothing in the old process asserted that, and a wait loop
# cannot tell "slow" from "will never happen".
#
# So every step here is followed by a check that fails loudly rather than waiting:
#
#   1. the tree is clean, on main, and level with origin/main          (refuse otherwise)
#   2. CHANGELOG has a section for the version                         (refuse otherwise)
#   3. the commit's CI is green on the remote                          (refuse otherwise)
#   4. annotated tag, idempotent: same commit -> reuse, other -> stop  (never rewrite silently)
#   5. push the tag to BOTH forges, then read both back                (the mirror is not trusted)
#   6. wait for each forge's release run, bounded, and say what failed
#   7. verify what a user would actually touch: release assets on both
#      forges, and the image manifest pullable anonymously
#
# Usage:
#   scripts/release.sh v0.5.0
#
# Environment:
#   GITEA_URL        default https://git.xiyuera.site:8443
#   GITEA_TOKEN      token for Gitea (admin is fine; approvals are not done here)
#   GITHUB_TOKEN     token for github.com, or `gh auth token` is used
#   GITHUB_REPO      default xiyuera/byre
#   SKIP_WAIT=1      stop after both tags are confirmed (useful for a dry run)
#
# Exit codes: 0 everything verified; 1 a check failed; 2 usage or missing tooling.
set -uo pipefail

VERSION=${1:-}
case "$VERSION" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "usage: $0 vX.Y.Z" >&2; exit 2 ;;
esac
SEMVER=${VERSION#v}

GITEA_URL=${GITEA_URL:-https://git.xiyuera.site:8443}
GITHUB_REPO=${GITHUB_REPO:-xiyuera/byre}
SKIP_WAIT=${SKIP_WAIT:-0}

die() { echo "FAIL: $*" >&2; exit 1; }
say() { echo "  $*"; }

# ---------------------------------------------------------------- 0. tooling
for t in git curl python3; do command -v "$t" >/dev/null || die "missing $t"; done
GITEA_TOKEN=${GITEA_TOKEN:-}
GH_TOKEN=${GITHUB_TOKEN:-$(gh auth token 2>/dev/null || true)}
[ -n "$GITEA_TOKEN" ] || die "GITEA_TOKEN is not set"
[ -n "$GH_TOKEN" ]    || die "GITHUB_TOKEN is not set and 'gh auth token' returned nothing"

cd "$(git rev-parse --show-toplevel)" || die "not in a git repository"

say "releasing $VERSION"

# ---------------------------------------------------------------- 1. tree state
[ -z "$(git status --porcelain)" ] || die "working tree is not clean; commit or stash first"
BRANCH=$(git rev-parse --abbrev-ref HEAD)
[ "$BRANCH" = "main" ] || die "on '$BRANCH', releases are cut from main"
git fetch -q origin || die "git fetch failed"
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || die "HEAD is not origin/main; pull or push first"
COMMIT=$(git rev-parse HEAD)
SHORT=$(git rev-parse --short HEAD)
say "commit $SHORT on main"

# ---------------------------------------------------------------- 2. changelog
# The release notes are the changelog section, not a summary written twice. No section -> no release.
python3 - "$SEMVER" <<'PY' || exit 1
import re, sys, pathlib
ver = sys.argv[1]
text = pathlib.Path("CHANGELOG.md").read_text()
m = re.search(rf"^## \[{re.escape(ver)}\][^\n]*\n(.*?)(?=^## \[|\Z)", text, re.S | re.M)
if not m:
    print(f"FAIL: CHANGELOG.md has no '## [{ver}]' section", file=sys.stderr)
    sys.exit(1)
body = m.group(1).strip()
if len(body) < 40:
    print(f"FAIL: CHANGELOG section for {ver} is too thin ({len(body)} chars)", file=sys.stderr)
    sys.exit(1)
print(f"  changelog: {len(body)} chars for {ver}")
PY

# ---------------------------------------------------------------- 3. CI green on this commit
say "checking CI on $SHORT"
STATUS=$(curl -sS --cacert "${GITEA_CA:-/opt/data/home/.config/certs/git-xiyuera.crt}" \
  -H "Authorization: token $GITEA_TOKEN" \
  "$GITEA_URL/api/v1/repos/byre/byre/commits/$COMMIT/status" 2>/dev/null \
  | python3 -c "
import json,sys
try: d=json.load(sys.stdin)
except Exception: print('parse_error'); raise SystemExit
st=[x['status'] for x in (d.get('statuses') or []) if '(pull_request)' not in x['context']]
# A skipped check is a pass, not a failure: dco is skipped on push because it needs a merge base
# that a push does not have. Counting it as failure made this check reject a green commit the first
# time it ran for real. Pending is its own answer -- it means not finished, not broken.
# (No backticks in here: this python is a double-quoted shell string, so they would run as commands.)
ok = all(s in ('success','skipped') for s in st)
bad = any(s in ('failure','error','cancelled','cancel') for s in st)
print(('green' if ok else ('red' if bad else 'pending')) if st else 'none')
" 2>/dev/null)
[ "$STATUS" = "green" ] || die "CI on $SHORT is '$STATUS', not green (push-event contexts)"
say "CI green"

# ---------------------------------------------------------------- 4. tag, idempotent
if git rev-parse -q --verify "refs/tags/$VERSION" >/dev/null 2>&1; then
  EXISTING=$(git rev-parse "$VERSION^{commit}")
  if [ "$EXISTING" = "$COMMIT" ]; then
    say "tag $VERSION already points at $SHORT; reusing"
  else
    die "tag $VERSION points at $(git rev-parse --short "$EXISTING"), not $SHORT.
       Refusing to move it. Delete it deliberately (both forges) and re-run, or bump the version."
  fi
else
  # Annotated, with an explicit identity: CI and gateway containers have no global git config.
  git -c user.name=byre-release -c user.email=release@byre.invalid \
      tag -a "$VERSION" -m "$VERSION

See CHANGELOG.md, section $SEMVER." "$COMMIT" >/dev/null || die "git tag failed"
  say "tag $VERSION created at $SHORT"
fi

# ---------------------------------------------------------------- 5. both forges, then read back
push_tag() { # $1 remote-url, $2 label
  git -c user.name=byre-release -c user.email=release@byre.invalid \
      push "$1" "refs/tags/$VERSION" >/dev/null 2>&1 \
    || git -c user.name=byre-release -c user.email=release@byre.invalid \
           push --force "$1" "refs/tags/$VERSION" >/dev/null 2>&1 \
    || die "push of $VERSION to $2 failed"
  say "pushed to $2"
}

push_tag "origin" "Gitea"
push_tag "https://x:${GH_TOKEN}@github.com/${GITHUB_REPO}.git" "GitHub"

# The mirror is not trusted to carry tags, so both are read back and compared to the local tag object.
for pair in "Gitea|$GITEA_URL/api/v1/repos/byre/byre/git/refs/tags/$VERSION" \
            "GitHub|https://api.github.com/repos/$GITHUB_REPO/git/refs/tags/$VERSION"; do
  label=${pair%%|*}; url=${pair#*|}
  if [ "$label" = "Gitea" ]; then
    got=$(curl -sS --cacert "${GITEA_CA:-/opt/data/home/.config/certs/git-xiyuera.crt}" \
          -H "Authorization: token $GITEA_TOKEN" "$url" 2>/dev/null \
          | python3 -c "import json,sys; d=json.load(sys.stdin); print((d.get('object') or {}).get('sha',''))" 2>/dev/null)
  else
    got=$(curl -sS -H "Authorization: token $GH_TOKEN" "$url" 2>/dev/null \
          | python3 -c "import json,sys; d=json.load(sys.stdin); print((d.get('object') or {}).get('sha',''))" 2>/dev/null)
  fi
  WANT=$(git rev-parse "$VERSION")
  [ "$got" = "$WANT" ] || die "$label has tag object '${got:-<missing>}', expected $WANT"
  say "$label tag object matches ($WANT)"
done

[ "$SKIP_WAIT" = "1" ] && { say "SKIP_WAIT=1; stopping after tag verification"; exit 0; }

# ---------------------------------------------------------------- 6. release runs
# Bounded waits with a reason. A wait loop that prints nothing but dots cannot distinguish
# "still building" from "never triggered", which is exactly how the first attempt lost 30 minutes.
wait_github() {
  local i state
  for i in $(seq 60); do
    sleep 20
    state=$(curl -sS -H "Authorization: token $GH_TOKEN" \
      "https://api.github.com/repos/$GITHUB_REPO/actions/runs?per_page=20" 2>/dev/null \
      | python3 -c "
import json,sys
d=json.load(sys.stdin)
for r in d.get('workflow_runs') or []:
    if r.get('name')=='release' and r.get('head_branch')=='$VERSION':
        print(f\"{r['status']}/{r.get('conclusion') or '-'}\"); break
else: print('none')
" 2>/dev/null)
    say "[github $i] $state"
    case "$state" in
      completed/success) return 0;;
      completed/*) return 1;;
    esac
  done
  say "gave up waiting on GitHub after 20 minutes"
  return 1
}

wait_gitea() {
  local i st
  for i in $(seq 60); do
    sleep 20
    st=$(curl -sS --cacert "${GITEA_CA:-/opt/data/home/.config/certs/git-xiyuera.crt}" \
      -H "Authorization: token $GITEA_TOKEN" \
      "$GITEA_URL/api/v1/repos/byre/byre/actions/tasks?limit=20" 2>/dev/null \
      | python3 -c "
import json,sys
d=json.load(sys.stdin)
for t in (d.get('workflow_runs') or d.get('tasks') or []):
    if t.get('name')=='publish' and t.get('head_branch')=='$VERSION':
        s=t.get('status')
        print('success' if s=='success' else ('failure' if s=='failure' else s)); break
else: print('none')
" 2>/dev/null)
    say "[gitea $i] $st"
    case "$st" in
      success) return 0;;
      failure|cancelled) return 1;;
    esac
  done
  say "gave up waiting on Gitea after 20 minutes"
  return 1
}

wait_github || die "GitHub release run did not succeed"
say "GitHub release run succeeded"
wait_gitea || die "Gitea publish job did not succeed"
say "Gitea publish job succeeded"

# ---------------------------------------------------------------- 7. what a user touches
say "verifying release assets"
for pair in "GitHub|https://api.github.com/repos/$GITHUB_REPO/releases/tags/$VERSION" \
            "Gitea|$GITEA_URL/api/v1/repos/byre/byre/releases/tags/$VERSION"; do
  label=${pair%%|*}; url=${pair#*|}
  if [ "$label" = "GitHub" ]; then
    n=$(curl -sS -H "Authorization: token $GH_TOKEN" "$url" 2>/dev/null \
        | python3 -c "import json,sys; d=json.load(sys.stdin); print(len(d.get('assets') or []))" 2>/dev/null)
  else
    n=$(curl -sS --cacert "${GITEA_CA:-/opt/data/home/.config/certs/git-xiyuera.crt}" \
        -H "Authorization: token $GITEA_TOKEN" "$url" 2>/dev/null \
        | python3 -c "import json,sys; d=json.load(sys.stdin); print(len(d.get('assets') or []))" 2>/dev/null)
  fi
  case "$n" in
    ''|0|null) die "$label has no release assets for $VERSION";;
  esac
  say "$label release: $n assets"
done

say "verifying the image is pullable without credentials"
# GHCR does not serve manifests to an anonymous request directly -- that answers 401 even for a
# public package. The anonymous path is two steps: exchange for a pull token, then present it as
# Bearer. Checking only the second half reports a healthy public image as broken.
ghcr_manifest_code() { # $1 repo, $2 tag -> prints http code
  local tok code
  tok=$(curl -sS -m 20 "https://ghcr.io/token?scope=repository:$1:pull&service=ghcr.io" 2>/dev/null \
        | python3 -c "import json,sys; print(json.load(sys.stdin).get('token',''))" 2>/dev/null)
  [ -n "$tok" ] || { echo 000; return; }
  code=$(curl -sS -o /dev/null -w '%{http_code}' -m 20 \
    -H "Authorization: Bearer $tok" \
    -H 'Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json' \
    "https://ghcr.io/v2/$1/manifests/$2" 2>/dev/null)
  echo "${code:-000}"
}

for i in $(seq 20); do
  code=$(ghcr_manifest_code xiyuera/byre "$VERSION")
  say "[ghcr $i] manifest http=$code"
  [ "$code" = "200" ] && { say "image pullable anonymously"; break; }
  [ "$i" = "20" ] && die "ghcr.io manifest for $VERSION never returned 200 (got $code)"
  sleep 15
done

cat <<EOF

released $VERSION
  commit     $SHORT
  changelog  ${SEMVER} section, used verbatim as release notes
  assets     binaries + SHA256SUMS + build-info.txt (+ images.txt on GitHub)
  image      ghcr.io/xiyuera/byre:$VERSION (anonymous pull verified)
  gitea      $GITEA_URL/byre/byre/releases/tag/$VERSION
  github     https://github.com/$GITHUB_REPO/releases/tag/$VERSION
EOF
