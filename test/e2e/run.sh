#!/usr/bin/env bash
# e2e entry point.
#
# Two modes:
#   1. cluster acceptance (available today): point it at a running byre and a registered client key.
#      BYRE_CFG=config.yaml BYRE_KEY=id_ed25519 test/e2e/run.sh
#   2. kind (not automated yet): build the image, load it into kind, apply deploy/ with the pod backend,
#      then run the same checks. Documented in docs/architecture.md; tracked in docs/roadmap.md.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"

if [ -n "${BYRE_CFG:-}" ] && [ -n "${BYRE_KEY:-}" ]; then
    exec "$here/cluster-acceptance.sh" "$BYRE_CFG" "$BYRE_KEY" "$@"
fi

cat >&2 <<'MSG'
No target given.

  BYRE_CFG=config.yaml BYRE_KEY=path/to/client_key test/e2e/run.sh [--ttl-ok]

A kind-based run is planned (docs/roadmap.md): it needs a container runtime to build and load the
image, so it is not wired into CI yet.
MSG
exit 2
