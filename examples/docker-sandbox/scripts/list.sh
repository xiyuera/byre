#!/bin/sh
# byre calls this once at startup to see what already exists, so a gateway restart can adopt
# live instances instead of leaking them. Print a JSON array; `[]` is a valid answer.
#
# Docker label keys cannot contain '/', so acquire.sh stores the fingerprint under
# byre.io.identity and this script maps it back to annotations."byre.io/identity".
set -eu
first=1
printf '['
for name in $(docker ps --filter "label=byre.sandbox=1" --format '{{.Names}}'); do
    addr=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$name" 2>/dev/null || true)
    ident=$(docker inspect -f '{{index .Config.Labels "byre.io.identity"}}' "$name" 2>/dev/null || true)
    [ -n "$addr" ] || continue
    [ $first -eq 1 ] || printf ','
    first=0
    case "$ident" in
        ""|"<no value>")
            printf '{"id":"%s","addr":"%s:22"}' "$name" "$addr" ;;
        *)
            printf '{"id":"%s","addr":"%s:22","annotations":{"byre.io/identity":"%s"}}' "$name" "$addr" "$ident" ;;
    esac
done
printf ']\n'
