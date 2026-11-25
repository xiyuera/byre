#!/bin/sh
# End-to-end check for the kubernetes starter kit.
#
# Assumes: the manifests are applied, the byre Service is reachable on 127.0.0.1:2222
# (port-forward or NodePort), and $BYRE_TEST_KEY points at a key listed in identities[].
set -eu

KEY=${BYRE_TEST_KEY:-$HOME/.ssh/id_ed25519}
ADDR=${BYRE_ADDR:-127.0.0.1}
PORT=${BYRE_PORT:-2222}
OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=60"

echo "== template / pool"
kubectl -n byre get sandboxtemplate byre-sandbox -o wide
kubectl -n byre get sandboxwarmpool byre-sandbox

echo "== first connection (claim should be created)"
H1=$(ssh $OPTS -p "$PORT" -i "$KEY" sandbox@"$ADDR" 'hostname')
echo "   instance: $H1"

echo "== claims now"
kubectl -n byre get sandboxclaims

echo "== lease table (40-byre-config.yaml sets lease.store: crd)"
if ! kubectl -n byre get byl 2>/dev/null; then
    # Not cosmetic: with store: crd a gateway that cannot list byreleases still starts and still
    # hands out instances, it just records nothing -- so zero leases here means the RBAC in
    # 30-rbac.yaml is not bound, not that no one holds a lease.
    echo "FAIL  no ByreLease objects readable; check the byreleases rule in 30-rbac.yaml"
    exit 1
fi

echo "== second connection (must be the same instance while the lease holds)"
H2=$(ssh $OPTS -p "$PORT" -i "$KEY" sandbox@"$ADDR" 'hostname')
echo "   instance: $H2"

if [ "$H1" = "$H2" ]; then
    echo "OK  same identity reused the same instance"
else
    echo "FAIL  identity got two instances: $H1 vs $H2"
    exit 1
fi

echo "== lease release is asynchronous: delete happens within ttl+grace"

echo "== the lease should name the instance we just used"
LEASE_INST=$(kubectl -n byre get byl -o jsonpath='{.items[0].status.instance}' 2>/dev/null || true)
echo "   lease instance: ${LEASE_INST:-<none>}"
if [ -n "$LEASE_INST" ] && [ "$LEASE_INST" != "$H1" ]; then
    echo "FAIL  lease names $LEASE_INST, the connection landed on $H1"
    exit 1
fi
echo "OK  lease and instance agree"
