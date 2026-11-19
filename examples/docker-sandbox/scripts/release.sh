#!/bin/sh
# byre calls this when a lease ends. It passes the instance id as BYRE_INSTANCE_ID (not as a
# positional argument) plus BYRE_INSTANCE_ADDR for logging -- see internal/backend/command.go.
# Idempotent: an instance that is already gone is not an error.
set -eu
ID=${BYRE_INSTANCE_ID:?release.sh reads BYRE_INSTANCE_ID from the environment}
docker rm -f "$ID" >/dev/null 2>&1 || true
