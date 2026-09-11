# Otter — build and packaging tasks.
#
# Everything here works with a plain `go` on PATH. Override any of these:
#   make build GO=/usr/local/go/bin/go
#   make docker VERSION=1.2.3

GO      ?= go
GOFLAGS ?=
BIN     ?= bin
EXAMPLES?= ./examples
DATA    ?= ./tmp
IMAGE   ?= otter

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# -X main.version=... overrides the `var version = "dev"` default in each
# command's main package. -s -w keeps the shipped binaries small.
LDFLAGS := -s -w -X main.version=$(VERSION)

# Every target runs in the repository root, so it works from any directory.
ROOT := $(CURDIR)

# Go source directories. Scoping tools to these keeps gofmt from walking
# unrelated trees such as a local module cache under .cache/.
GO_DIRS := ./cmd ./internal ./sdk ./migrations

# The Go SQLite driver is pure Go (modernc.org/sqlite); no cgo is required.
export CGO_ENABLED = 0

.DEFAULT_GOAL := help

.PHONY: help build test lint run example cross docker clean fmt tidy clean-pycache

## help: list the available targets (default goal)
help:
	@echo "Otter build targets (VERSION=$(VERSION))"
	@echo ""
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed -e 's/^## //' | awk -F': ' '{ printf "  %-10s %s\n", $$1, $$2 }'
	@echo ""

## build: build ./bin/otterd and ./bin/otter with VERSION injected
build: clean-pycache
	@mkdir -p $(BIN)
	$(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/otterd ./cmd/otterd
	$(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/otter ./cmd/otter
	@echo "built $(BIN)/otterd and $(BIN)/otter ($(VERSION))"

# The Python SDK is embedded into the daemon with `go:embed all:python`, which
# includes every file in the tree. Bytecode caches left behind by running the
# SDK test suite would otherwise be compiled into the binary, so drop them
# before building.
.PHONY: clean-pycache
clean-pycache:
	@find . -name .cache -prune -o -name '__pycache__' -type d -exec rm -rf {} + 2>/dev/null || true

## test: run the full Go test suite
test:
	$(GO) test ./...

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

## run: build, then run otterd against ./examples with data in ./tmp
run: build
	$(BIN)/otterd --integrations $(EXAMPLES) --data $(DATA)

## example: build, then run otterd on ./examples with pretty logs
example: build
	$(BIN)/otterd --integrations $(EXAMPLES) --data $(DATA) --log-format pretty

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
.PHONY: cross-build
cross-build:
	GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 \
		$(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/otterd$(SUFFIX) ./cmd/otterd
	GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 \
		$(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/otter$(SUFFIX) ./cmd/otter

## docker: build the runtime image as $(IMAGE):$(VERSION) (run make build first)
docker:
	docker build -t $(IMAGE):$(VERSION) .

## clean: remove ./bin and ./tmp
clean:
	rm -rf $(ROOT)/$(BIN) $(ROOT)/$(DATA)

## fmt: rewrite all Go files with gofmt
fmt:
	gofmt -w $(GO_DIRS)

## tidy: sync go.mod/go.sum
tidy:
	$(GO) mod tidy
