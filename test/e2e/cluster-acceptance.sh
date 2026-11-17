#!/usr/bin/env bash
# Cluster acceptance: runs the charter's checks against a live byre + cluster.
#
# Usage:
#   test/e2e/cluster-acceptance.sh <config.yaml> <client-key> [--ttl-ok]
#
# It expects byre to be running with that config and the client key to be a registered identity.
# It asserts AC1 (the instance does not change inside the lease) and that -L reaches the instance,
# and prints pool/adoption numbers without asserting them: adoption needs a restart, which this
# script does not perform. --ttl-ok adds the 320s wait (~6 minutes).
set -euo pipefail

CFG="${1:?config path required}"
KEY="${2:?client private key required}"
LONG="${3:-}"

ADDR="${BYRE_ADDR:-127.0.0.1:2222}"
METRICS="${BYRE_METRICS:-http://127.0.0.1:8099/metrics}"
SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=20)

pass=0
fail=0
check() { # name expected actual
    if [ "$2" = "$3" ]; then printf '  PASS  %s\n' "$1"; pass=$((pass + 1));
    else printf '  FAIL  %s (want %s, got %s)\n' "$1" "$2" "$3"; fail=$((fail + 1)); fi
}
check_nonempty() { # name value
    if [ -n "$2" ]; then printf '  PASS  %s\n' "$1"; pass=$((pass + 1));
    else printf '  FAIL  %s (empty)\n' "$1"; fail=$((fail + 1)); fi
}

hostname_once() { ssh -p "${ADDR##*:}" -i "$KEY" "${SSH_OPTS[@]}" "root@${ADDR%%:*}" hostname 2>/dev/null | tail -1; }
metric() { curl -fsS --max-time 3 "$METRICS" | awk -v k="$1" '$1==k {print $2}'; }

echo "== AC1: same identity, same instance"
A=$(hostname_once)
check_nonempty "first connection returns a hostname" "$A"
if [ "$LONG" = "--ttl-ok" ]; then
    sleep 320
fi
B=$(hostname_once)
check "reconnect returns the same instance" "$A" "$B"

echo "== AC4: -L reaches the instance"
SOCK=$(mktemp -u /tmp/byre-e2e.XXXXXX)
ssh -f -N -M -S "$SOCK" -p "${ADDR##*:}" -i "$KEY" "${SSH_OPTS[@]}" -L 18022:127.0.0.1:22 "root@${ADDR%%:*}"
BANNER=$( (exec 3<>/dev/tcp/127.0.0.1/18022; head -c 24 <&3) 2>/dev/null || true )
ssh -S "$SOCK" -O exit "root@${ADDR%%:*}" >/dev/null 2>&1 || true
case "$BANNER" in SSH-*) check "tunnel carries the instance sshd banner" yes yes ;; *) check "tunnel carries the instance sshd banner" yes no ;; esac

echo "== AC5: adoption (not asserted by this script)"
ADOPTED=$(metric byre_leases_adopted || echo 0)
printf '  INFO  leases adopted at startup: %s\n' "${ADOPTED:-0}"
printf '  NOTE  adoption needs a restart with a live lease, which this script does not do:\n'
printf '        restart the gateway, then look for "adopted existing leases" in its log.\n'

echo "== pool"
WARM=$(metric 'byre_pool_warm{backend="sandbox"}')
printf '  INFO  warm instances: %s\n' "${WARM:-unknown}"

echo
printf '  passed=%d failed=%d\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
