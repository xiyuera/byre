SHELL      := /bin/sh
BIN        := bin/byre
PKG        := ./...
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X byre/internal/version.Version=$(VERSION) \
	-X byre/internal/version.Commit=$(COMMIT) \
	-X byre/internal/version.BuildDate=$(BUILD_DATE)

.PHONY: all build test test-race lint fmt vet tidy clean e2e check-config render image image-push help

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/byre
	@echo "built $(BIN) ($(VERSION))"

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
	docker push $(IMAGE_REGISTRY)/$(IMAGE)
	@echo "pushed $(IMAGE_REGISTRY)/$(IMAGE)"

clean:
	rm -rf bin dist coverage.txt

help:
	@grep -E '^[a-z-]+:' $(MAKEFILE_LIST) | sed 's/:.*//' | sort -u | tr '\n' ' '; echo
