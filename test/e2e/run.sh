#!/bin/sh
# End-to-end gate. Deliberately not implemented yet: M5 owns it, and a gate that silently passes is
# worse than no gate.
#
# Planned (see docs/architecture.md and the project charter):
#   AC1  same identity across connections -> same instance hostname
#   AC2  two identities -> two different instances
#   AC3  idle > ttl+grace -> instance gone, pool back to pool_size
#   AC4  ssh -L through the gateway reaches the instance's local port
#   AC5  gateway restart -> instance count does not grow (adoption)
#   AC6  a credential file synced by the client is usable inside the instance
#   AC7  six identities concurrently -> six distinct instances
#
# Shape of the real script:
#   kind create cluster
#   docker build -t keypod:e2e .
#   kind load docker-image keypod:e2e
#   kubectl apply -k deploy/            # with an e2e overlay: pod-image backend, 2 identities
#   run the checks above with ssh + kubectl
#   kind delete cluster
set -eu
echo "e2e not implemented yet (M5). See the checklist in this script and docs/architecture.md." >&2
exit 1
