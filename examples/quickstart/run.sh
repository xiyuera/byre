#!/bin/sh
# One command from nothing to a working SSH gateway.
#
#   ./run.sh          start it (idempotent: keys and image are reused)
#   ./run.sh status   what is running
#   ./run.sh ssh      open a connection
#   ./run.sh down     remove the containers, keep the keys
#   ./run.sh clean    remove everything including the generated keys
#
# Nothing here edits a file by hand: the fingerprint, the pool key and the host key are all
# generated into .work/ on first run. That is the part every other example asks you to do
# manually, and it is the part that makes a first run take minutes instead of seconds.
set -eu
cd "$(dirname "$0")"

WORK=.work
IMAGE=${BYRE_IMAGE:-ghcr.io/xiyuera/byre:latest}
NET=byre-quickstart
SANDBOX=sandbox            # also the hostname the config dials: endpoints: ["sandbox:22"]
GATEWAY=byre-quickstart
SANDBOX_TAG=byre-quickstart-sandbox

say() { printf '  %s\n' "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

need_docker() {
  command -v docker >/dev/null 2>&1 || die "docker is not installed"
  docker info >/dev/null 2>&1 || die "cannot talk to the docker daemon"
}

client_key() { echo "$WORK/client/byre_client"; }

wait_for_gateway() {
  i=0
  while [ "$i" -lt 30 ]; do
    if docker logs "$GATEWAY" 2>&1 | grep -qiE 'listening|listen'; then return 0; fi
    if [ "$(docker inspect -f '{{.State.Running}}' "$GATEWAY" 2>/dev/null || echo false)" != "true" ]; then
      echo "--- gateway log ---" >&2
      docker logs "$GATEWAY" 2>&1 | tail -25 >&2
      die "the gateway exited"
    fi
    i=$((i + 1)); sleep 1
  done
  echo "--- gateway log ---" >&2
  docker logs "$GATEWAY" 2>&1 | tail -25 >&2
  die "the gateway did not start listening within 30s"
}

up() {
  need_docker
  mkdir -p "$WORK/keys" "$WORK/client" "$WORK/sandbox"

  # 1. The pool key: the gateway authenticates to instances with this. Its public half is baked
  #    into the sandbox image at build time, so it must exist before the build.
  if [ ! -f "$WORK/keys/id_key" ]; then
    ssh-keygen -t ed25519 -N '' -C 'byre quickstart pool key' -f "$WORK/keys/id_key" -q
    say "generated pool key"
  fi
  # The gateway runs as an unprivileged user inside its image; a 600 key owned by the host user is
  # unreadable to it. This key only reaches a throwaway container on a private bridge network.
  chmod 644 "$WORK/keys/id_key"

  # 2. Your client key: the one you will actually connect with.
  if [ ! -f "$(client_key)" ]; then
    ssh-keygen -t ed25519 -N '' -C 'byre quickstart client key' -f "$(client_key)" -q
    say "generated client key: $(client_key)"
  fi

  # 3. Render the config. This is the only value that differs per user.
  fp=$(ssh-keygen -lf "$(client_key).pub" -E sha256 | awk '{print $2}')
  sed "s|__FINGERPRINT__|$fp|" config.template.yaml > "$WORK/config.yaml"
  say "identity: $fp"

  # 4. Sandbox image. The Dockerfile expects the pool public key in its build context; rewriting
  #    that one path lets the whole context be .work/sandbox instead of the docker-sandbox example.
  sed 's|keys/id_key.pub|id_key.pub|' ../docker-sandbox/Dockerfile.sandbox > "$WORK/sandbox/Dockerfile"
  cp "$WORK/keys/id_key.pub" "$WORK/sandbox/id_key.pub"
  if ! docker image inspect "$SANDBOX_TAG" >/dev/null 2>&1; then
    say "building sandbox image (first run only)"
    docker build -q -t "$SANDBOX_TAG" "$WORK/sandbox"
  fi

  # 5. Network + sandbox.
  docker network create "$NET" >/dev/null 2>&1 || true
  docker rm -f "$SANDBOX" >/dev/null 2>&1 || true
  docker run -d --name "$SANDBOX" --network "$NET" "$SANDBOX_TAG" >/dev/null
  say "sandbox container up"

  # 6. The gateway.
  docker rm -f "$GATEWAY" >/dev/null 2>&1 || true
  if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
    say "pulling $IMAGE"
    docker pull -q "$IMAGE" >/dev/null
  fi
  docker run -d --name "$GATEWAY" --network "$NET" \
    -p 2222:2222 \
    -v "$(pwd)/$WORK/config.yaml:/etc/byre/config.yaml:ro" \
    -v "$(pwd)/$WORK/keys/id_key:/home/byre/id_key:ro" \
    "$IMAGE" >/dev/null
  wait_for_gateway
  say "gateway up on :2222"

  cat <<EOF

Connect:

  ssh -o StrictHostKeyChecking=no -o IdentitiesOnly=yes \\
      -p 2222 -i $(pwd)/$(client_key) sandbox@127.0.0.1

Connect twice. Both land on the same instance while the lease (120s) is alive, because the lease is
keyed on the client key, not on the connection. Wait out the ttl and it is released.

  ./run.sh status    containers and the gateway's own view
  ./run.sh down      stop, keep keys (the next ./run.sh reuses them)
  ./run.sh clean     remove .work/ as well
EOF
}

status() {
  need_docker
  docker ps --filter "network=$NET" --format '  {{.Names}}\t{{.Status}}\t{{.Ports}}' || true
  echo
  echo "  gateway log (last 12 lines):"
  docker logs "$GATEWAY" 2>&1 | tail -12 | sed 's/^/    /' || true
}

do_ssh() {
  exec ssh -o StrictHostKeyChecking=no -o IdentitiesOnly=yes -p 2222 -i "$(pwd)/$(client_key)" sandbox@127.0.0.1
}

down() {
  need_docker
  docker rm -f "$GATEWAY" "$SANDBOX" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  say "containers removed; keys kept in $WORK/"
}

clean() {
  down
  rm -rf "$WORK"
  docker rmi -f "$SANDBOX_TAG" >/dev/null 2>&1 || true
  say "removed $WORK/ and the sandbox image"
}

case ${1:-up} in
  up|start) up ;;
  status|ps) status ;;
  ssh) do_ssh ;;
  down|stop) down ;;
  clean|rm) clean ;;
  *) die "usage: $0 [up|status|ssh|down|clean]" ;;
esac
