#!/bin/sh
# Generate the pool key pair. The gateway keeps the private half and authenticates to every
# instance with it; the image bakes in the public half. Run this before `docker build`.
set -eu
cd "$(dirname "$0")/.."
mkdir -p keys
if [ -f keys/id_key ]; then
    echo "keys/id_key already exists; leaving it alone"
    exit 0
fi
ssh-keygen -t ed25519 -N '' -C 'byre docker example pool key' -f keys/id_key -q
chmod 600 keys/id_key
echo "wrote keys/id_key and keys/id_key.pub"
