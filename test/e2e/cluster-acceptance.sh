#!/usr/bin/env bash
# Cluster acceptance: runs the charter's checks against a live byre + cluster.
#
# Usage:
#   test/e2e/cluster-acceptance.sh <config.yaml> <client-key> [--ttl-ok] [--allow-na]
#
# It expects byre to be running with that config and the client key to be a registered identity.
# AC1/AC3 need a long wall-clock wait; pass --ttl-ok only when you accept that (~6 minutes).
#
# Two rules keep this script honest as evidence:
#   * no check compares a value with itself: an empty $A (ssh failed) must fail AC1, not pass it;
#   * AC5 can only be observed after a restart that adopted live instances, so when it cannot be
#     evaluated it is reported as N/A and the run exits non-zero. An N/A is not a pass; --allow-na
#     exists for a run that is explicitly not claiming AC5.
set -euo pipefail

CFG="${1:?config path required}"
KEY="${2:?client private key required}"
shift 2
LONG=""
ALLOW_NA=0
for arg in "$@"; do
    case "$arg" in
        "") ;; # run.sh forwards an empty $1 when no flag was given
        --ttl-ok) LONG=1 ;;
        --allow-na) ALLOW_NA=1 ;;
        *) echo "unknown argument: $arg" >&2; exit 2 ;;
    esac
done

ADDR="${BYRE_ADDR:-127.0.0.1:2222}"
METRICS="${BYRE_METRICS:-http://127.0.0.1:8099/metrics}"
SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=20)

pass=0
fail=0
na=0
check() { # name expected actual
    if [ "$2" = "$3" ]; then printf '  PASS  %s\n' "$1"; pass=$((pass + 1));
    else printf '  FAIL  %s (want %s, got %s)\n' "$1" "$2" "$3"; fail=$((fail + 1)); fi
}
skip() { # name reason
    printf '  N/A   %s (%s)\n' "$1" "$2"; na=$((na + 1));
}

hostname_once() { ssh -p "${ADDR##*:}" -i "$KEY" "${SSH_OPTS[@]}" "root@${ADDR%%:*}" hostname 2>/dev/null | tail -1; }
metric() { curl -fsS --max-time 3 "$METRICS" | awk -v k="$1" '$1==k {print $2}'; }

echo "== AC1: same identity, same instance"
A=$(hostname_once)
# The value must exist before it can be compared: an ssh failure used to make this line pass.
check "first connection returns an instance hostname" "yes" "$([ -n "$A" ] && echo yes || echo no)"
if [ "$LONG" = "1" ]; then
    sleep 320
fi
B=$(hostname_once)
if [ -z "$A" ]; then
    skip "reconnect returns the same instance" "no first connection to compare against"
else
    check "reconnect returns the same instance" "$A" "$B"
fi

echo "== AC4: -L reaches the instance"
SOCK=$(mktemp -u /tmp/byre-e2e.XXXXXX)
ssh -f -N -M -S "$SOCK" -p "${ADDR##*:}" -i "$KEY" "${SSH_OPTS[@]}" -L 18022:127.0.0.1:22 "root@${ADDR%%:*}"
BANNER=$( (exec 3<>/dev/tcp/127.0.0.1/18022; head -c 24 <&3) 2>/dev/null || true )
ssh -S "$SOCK" -O exit "root@${ADDR%%:*}" >/dev/null 2>&1 || true
case "$BANNER" in SSH-*) check "tunnel carries the instance sshd banner" yes yes ;; *) check "tunnel carries the instance sshd banner" yes no ;; esac

echo "== AC5: adoption"
ADOPTED=$(metric byre_leases_adopted || true)
if [ -z "${ADOPTED}" ]; then
    skip "leases adopted at startup" "byre_leases_adopted is not readable at ${METRICS}"
elif [ "${ADOPTED}" -ge 1 ]; then
    check "leases adopted at startup" "yes" "yes"
else
    skip "leases adopted at startup" "0 adopted: restart the gateway while a lease is live, then re-run"
fi

echo "== pool"
WARM=$(metric 'byre_pool_warm{backend="sandbox"}')
printf '  INFO  warm instances: %s\n' "${WARM:-unknown}"

echo
printf '  passed=%d failed=%d na=%d\n' "$pass" "$fail" "$na"
[ "$fail" -eq 0 ] || exit 1
if [ "$na" -ne 0 ] && [ "$ALLOW_NA" -ne 1 ]; then
    echo "  an N/A is not a pass: run the checks above, or pass --allow-na and do not cite them as evidence" >&2
    exit 1
fi
