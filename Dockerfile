# syntax=docker/dockerfile:1
#
# Two stages, no shell in the runtime image's dependency chain beyond busybox: the gateway is a single
# static binary, so nothing else needs to be there.
FROM golang:1.23-alpine AS build

WORKDIR /src

# Dependencies first: this layer only changes when go.mod/go.sum do.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w \
        -X byre/internal/version.Version=${VERSION} \
        -X byre/internal/version.Commit=${COMMIT} \
        -X byre/internal/version.BuildDate=${BUILD_DATE}" \
      -o /out/byre ./cmd/byre

FROM alpine:3.20

RUN apk add --no-cache ca-certificates \
 && adduser -D -u 10001 -h /home/byre byre

COPY --from=build /out/byre /usr/local/bin/byre

# accept-new host keys are written relative to HOME, which must be writable; the deployment mounts a
# tmpfs/emptyDir here when the root filesystem is read-only.
ENV HOME=/home/byre
USER byre:byre

EXPOSE 2222 8099

ENTRYPOINT ["/usr/local/bin/byre"]
CMD ["-config", "/etc/byre/config.yaml"]
