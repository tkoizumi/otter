# Otter — contributor build and packaging tasks.
#
# This repository is the Otter runtime: the daemon, the CLI, the embedded Python
# SDK and their contracts. It is not an integration project.
#
# To *use* Otter, install it and work in your own directory — `otter init`,
# `otter start`, `otter run`. To work *on* Otter, these targets are the
# supported loop. See CONTRIBUTING.md.
#
# Everything here works with a plain `go` on PATH. Override any of these:
#   make build GO=/usr/local/go/bin/go

GO      ?= go
GOFLAGS ?=
BIN     ?= bin

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# -X main.version=... overrides the `var version = "dev"` default in each
# command's main package. -s -w keeps the shipped binaries small.
LDFLAGS := -s -w -X main.version=$(VERSION)

# Every target runs in the repository root, so it works from any directory.
ROOT := $(CURDIR)

# The binary the smoke workflow drives. It is deliberately this checkout's
# build, by absolute path: a contributor's PATH often resolves `otter` to an
# installed release, and the smoke test is about this tree.
OTTER ?= $(ROOT)/$(BIN)/otter

# Go source directories. Scoping tools to these keeps gofmt from walking
# unrelated trees such as a local module cache under .cache/.
GO_DIRS := ./cmd ./internal ./sdk ./migrations

# The Go SQLite driver is pure Go (modernc.org/sqlite); no cgo is required.
export CGO_ENABLED = 0

.DEFAULT_GOAL := help
.PHONY: help build test test-go test-python smoke lint fmt tidy clean clean-pycache cross cross-build

## help: list the available targets (default goal)
help:
	@echo "Otter runtime build targets (VERSION=$(VERSION))"
	@echo ""
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed -e 's/^## //' | awk -F': ' '{ printf "  %-18s %s\n", $$1, $$2 }'
	@echo ""
	@echo "This checkout is the runtime, not an integration project. Using Otter"
	@echo "means installing it and running it in your own directory: otter init,"
	@echo "otter release, otter start, otter run, otter status, otter stop."

## build: build ./bin/otterd and ./bin/otter with VERSION injected
build: clean-pycache
	@mkdir -p $(BIN)
	$(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/otterd ./cmd/otterd
	$(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/otter ./cmd/otter
	@echo "built $(BIN)/otterd and $(BIN)/otter ($(VERSION))"

# The Python SDK is embedded into the daemon with `go:embed all:python`, which
# includes every file in that tree. Bytecode caches left behind by the SDK test
# suite would otherwise be compiled into the binary, so drop them before
# building. The scope is `sdk/python`, the only embedded tree.
clean-pycache:
	@find sdk/python -name '__pycache__' -type d -exec rm -rf {} + 2>/dev/null || true

## test: run every suite (Go plus the embedded SDK)
test: test-go test-python

## test-go: run the Go test suite
test-go:
	$(GO) test ./...

## test-python: run the embedded Python SDK suite
test-python:
	@python3 -m unittest discover -s sdk/python/tests

## smoke: init, release, run and read back state in a temporary workspace
smoke: build
	@OTTER_BIN=$(OTTER) sh $(ROOT)/scripts/smoke.sh

## lint: gofmt check, go vet, and golangci-lint when it is installed
lint:
	@files="$$(gofmt -l $(GO_DIRS) || true)"; \
	if [ -n "$$files" ]; then \
		echo "gofmt needed for:"; echo "$$files"; exit 1; \
	fi
	$(GO) vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not found on PATH — skipping (install it for the full lint pass)"; \
	fi

## cross: build linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 into ./bin
cross:
	@mkdir -p $(BIN)
	$(MAKE) --no-print-directory cross-build GOOS=linux   GOARCH=amd64 SUFFIX=-linux-amd64
	$(MAKE) --no-print-directory cross-build GOOS=linux   GOARCH=arm64 SUFFIX=-linux-arm64
	$(MAKE) --no-print-directory cross-build GOOS=darwin  GOARCH=arm64 SUFFIX=-darwin-arm64
	$(MAKE) --no-print-directory cross-build GOOS=darwin  GOARCH=amd64 SUFFIX=-darwin-amd64
	@echo "cross builds in $(BIN)/:"
	@ls -1 $(BIN) | grep -E '^otter(d)?-' || true

# Helper for `cross`; not part of the public interface.
cross-build:
	GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 \
		$(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/otterd$(SUFFIX) ./cmd/otterd
	GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 \
		$(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/otter$(SUFFIX) ./cmd/otter

## clean: remove declared build artifacts (./bin, ./dist); never runtime state
clean:
	rm -rf $(ROOT)/$(BIN) $(ROOT)/dist

## fmt: rewrite all Go files with gofmt
fmt:
	gofmt -w $(GO_DIRS)

## tidy: sync go.mod/go.sum
tidy:
	$(GO) mod tidy
