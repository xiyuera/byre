SHELL      := /bin/sh
BIN        := bin/keypod
PKG        := ./...
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X keypod/internal/version.Version=$(VERSION) \
	-X keypod/internal/version.Commit=$(COMMIT) \
	-X keypod/internal/version.BuildDate=$(BUILD_DATE)

.PHONY: all build test test-race lint fmt vet tidy clean e2e check-config help

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/keypod
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
	$(BIN) -config $(CONFIG) -check-config

e2e:
	./test/e2e/run.sh

clean:
	rm -rf bin dist coverage.txt

help:
	@grep -E '^[a-z-]+:' $(MAKEFILE_LIST) | sed 's/:.*//' | sort -u | tr '\n' ' '; echo
