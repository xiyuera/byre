SHELL      := /bin/sh
BIN        := bin/byre
PKG        := ./...
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
COMMIT_FULL ?= $(shell git rev-parse HEAD 2>/dev/null || echo none)
# The stamped date is the commit time, in UTC, read once. It is a property of the commit, not of the
# clock on the machine that builds it: two builds of one commit have to produce identical bytes.
# `date -u` here meant every build differed, and the release job read it once per architecture, so the
# two binaries of a single tag were stamped two seconds apart and could never match a rebuild.
BUILD_DATE  ?= $(shell TZ=UTC git log -1 --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)

# One line on purpose: the same string is written into build-info.txt and has to be copy-pasteable.
LDFLAGS := -s -w -X byre/internal/version.Version=$(VERSION) -X byre/internal/version.Commit=$(COMMIT) -X byre/internal/version.BuildDate=$(BUILD_DATE)

# Release matrix. All architectures are built in one invocation so they share one BUILD_DATE. The job
# image is recorded because two toolchains build the same source into different bytes: without it a
# reader cannot tell a rebuild that differs because of the compiler from one that differs because of
# the flags.
DIST_ARCHES     ?= arm64 amd64
CI_IMAGE        ?= unset
CI_IMAGE_DIGEST ?= unset

.PHONY: all build dist test test-race lint fmt vet tidy clean e2e check-config render image image-push image-verify help

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/byre
	@echo "built $(BIN) ($(VERSION))"

# Everything a release ships, from the same inputs: two static binaries, build-info.txt and SHA256SUMS.
# The workflow calls this instead of writing its own build loop, so "what the pipeline does" and "what
# a person reproduces locally" cannot drift apart.
dist:
	@rm -rf dist && mkdir -p dist
	@for arch in $(DIST_ARCHES); do \
		echo "build dist/byre-$(VERSION)-linux-$$arch"; \
		GOOS=linux GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' \
			-o dist/byre-$(VERSION)-linux-$$arch ./cmd/byre || exit 1; \
	done
	@set -e; { \
		echo "version=$(VERSION)"; \
		echo "commit=$(COMMIT_FULL)"; \
		echo "commit_short=$(COMMIT)"; \
		echo "tag=$(VERSION)"; \
		echo "BuildDate=$(BUILD_DATE)"; \
		echo "go_toolchain=$$(go version)"; \
		echo "GOOS=linux"; \
		for a in $(DIST_ARCHES); do echo "asset=byre-$(VERSION)-linux-$$a GOOS=linux GOARCH=$$a"; done; \
		echo "ldflags=$(LDFLAGS)"; \
		echo "ci_image=$(CI_IMAGE)"; \
		echo "ci_image_digest=$(CI_IMAGE_DIGEST)"; \
		echo "reproduce=git clone <repository> && cd byre && git checkout $(COMMIT_FULL) && make dist VERSION=$(VERSION) COMMIT=$(COMMIT) BUILD_DATE=$(BUILD_DATE) CI_IMAGE=$(CI_IMAGE) CI_IMAGE_DIGEST=$(CI_IMAGE_DIGEST)"; \
	} > dist/build-info.txt
	@cd dist && sha256sum byre-* build-info.txt > SHA256SUMS && cat SHA256SUMS

test:
	go test $(PKG)

test-race:
	go test -race $(PKG)

fmt:
	gofmt -l -w .

vet:
	go vet $(PKG)

lint: fmt vet
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run $(PKG); \
	else \
		echo "golangci-lint not installed - ran gofmt+vet only"; \
	fi

tidy:
	go mod tidy

check-config: build
	@test -n "$(CONFIG)" || { echo "usage: make check-config CONFIG=path/to/config.yaml"; exit 2; }
	$(BIN) -config $(CONFIG) -check-config

e2e:
	@BYRE_CFG=$(CONFIG) BYRE_KEY=$(KEY) ./test/e2e/run.sh

IMAGE          ?= byre:$(VERSION)
# Override with your own registry: make image-push IMAGE_REGISTRY=registry.example.com/team
IMAGE_REGISTRY ?= localhost:5000

# Renders deploy/ with the same engine `kubectl apply -k` uses. Catches a broken kustomization before
# a cluster sees it; skipped with a warning when kustomize is not installed.
render:
	@if command -v kustomize >/dev/null 2>&1; then \
		kustomize build deploy/ >/dev/null && echo "deploy/ renders"; \
	else \
		echo "kustomize not installed - skipped (kubectl kustomize deploy/ would do the same)"; \
	fi

image:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(IMAGE) .
	@echo "built $(IMAGE)"

image-push: image
	docker tag $(IMAGE) $(IMAGE_REGISTRY)/$(IMAGE)
	@# push, then record the digest the registry assigned — a tag in the output is not evidence
	scripts/publish-image.sh $(IMAGE_REGISTRY)/$(IMAGE)
	@echo "pushed $(IMAGE_REGISTRY)/$(IMAGE) — IMAGE_REF recorded in dist/images.txt"

# Fails when the running pod's imageID is not the digest dist/images.txt records. The silent failure
# this catches: a re-pushed tag with imagePullPolicy: IfNotPresent keeps the node's old layers.
image-verify:
	scripts/assert-image-digest.sh $(or $(IMAGES),dist/images.txt) $(or $(NAMESPACE),byre-system) $(or $(DEPLOY),byre)

clean:
	rm -rf bin dist coverage.txt

help:
	@grep -E '^[a-z-]+:' $(MAKEFILE_LIST) | sed 's/:.*//' | sort -u | tr '\n' ' '; echo
