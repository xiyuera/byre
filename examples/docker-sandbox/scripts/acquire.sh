#!/bin/sh
# byre `command` backend: start one sandbox and print its address as a single JSON object on
# stdout. Lease, TTL and reuse are the gateway's job -- this script is stateless on purpose.
#
# byre passes BYRE_IDENTITY (the fingerprint of the key that asked for the lease). Recording
# it lets list.sh hand the instance back to the same identity after a gateway restart instead
# of leaking it (ADR-0009).
set -eu

IMAGE=${BYRE_SANDBOX_IMAGE:-byre-sandbox:local}
NETWORK=${BYRE_SANDBOX_NETWORK:-byre-example}
IDENTITY=${BYRE_IDENTITY:-}
NAME="byre-sbx-$(date +%s)-$$"

# Docker label keys may not contain '/', so the annotation is stored as byre.io.identity and
# mapped back in list.sh.
if [ -n "$IDENTITY" ]; then
    IDENT_LABEL="--label=byre.io.identity=$IDENTITY"
else
    IDENT_LABEL=""
fi

# shellcheck disable=SC2086
docker run -d \
  --name "$NAME" \
  --label "byre.sandbox=1" \
  $IDENT_LABEL \
  --network "$NETWORK" \
  --read-only \
  --tmpfs /run:rw,noexec,nosuid,size=16m \
  --cap-drop ALL \
  --cap-add SYS_CHROOT \
  --cap-add SETUID \
  --cap-add SETGID \
  --cap-add CHOWN \
  --cap-add DAC_OVERRIDE \
  --cap-add KILL \
  --security-opt no-new-privileges \
  "$IMAGE" >/dev/null

i=0
while [ $i -lt 40 ]; do
    if docker exec "$NAME" nc -z 127.0.0.1 22 2>/dev/null; then break; fi
    i=$((i+1)); sleep 0.5
done
if [ $i -ge 40 ]; then
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    echo "sandbox $NAME did not start sshd in 20s" >&2
    exit 1
fi

ADDR=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$NAME")
[ -n "$ADDR" ] || { docker rm -f "$NAME" >/dev/null 2>&1 || true; echo "no ip for $NAME" >&2; exit 1; }

if [ -n "$IDENTITY" ]; then
    printf '{"id":"%s","addr":"%s:22","annotations":{"byre.io/identity":"%s"}}\n' "$NAME" "$ADDR" "$IDENTITY"
else
    printf '{"id":"%s","addr":"%s:22"}\n' "$NAME" "$ADDR"
fi
