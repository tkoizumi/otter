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

# The two binaries every other target invokes. OTTER is what a developer uses;
# OTTERD is the daemon, for `cross` and for scripts that predate `otter start`.
OTTER  ?= $(BIN)/otter
OTTERD ?= $(BIN)/otterd

# Go source directories. Scoping tools to these keeps gofmt from walking
# unrelated trees such as a local module cache under .cache/.
GO_DIRS := ./cmd ./internal ./sdk ./migrations

# The Go SQLite driver is pure Go (modernc.org/sqlite); no cgo is required.
export CGO_ENABLED = 0

.DEFAULT_GOAL := help

.PHONY: help build test test-go test-python lint cross docker clean fmt tidy clean-pycache
.PHONY: start start-detached stop restart release schema
.PHONY: deploy deploy-plan deploy-status deploy-tunnel deploy-remote-runs deploy-destroy deploy-purge
.PHONY: deploy deploy-plan deploy-status deploy-tunnel deploy-remote-runs deploy-destroy deploy-purge

## help: list the available targets (default goal)
help:
	@echo "Otter build targets (VERSION=$(VERSION))"
	@echo ""
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed -e 's/^## //' | awk -F': ' '{ printf "  %-18s %s\n", $$1, $$2 }'
	@echo ""
	@echo "Integration operations take INTEGRATION=$(INTEGRATION) (any directory"
	@echo "under $(INTEGRATIONS)); runtime state lives in $(DATA)."
	@echo ""
	@echo "Running the runtime, and everything you do to it once it is up, is the"
	@echo "otter command, not make: otter start | stop | run | logs | state | status."
	@echo "otter.env and otter.daemon.env are loaded by otter start."
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

## test: run every suite (Go plus the Python packages)
test: test-go test-python

## test-go: run the Go test suite
test-go:
	$(GO) test ./...

## test-python: run the Python suites (runtime SDK, shared library, integrations)
test-python:
	@python3 -m unittest discover -s sdk/python/tests
	@echo
	@PYTHONPATH=lib/python python3 -m unittest discover -s lib/python/tests
	@for dir in integrations/*/tests; do \
		[ -d "$$dir" ] || continue; \
		echo; echo "== $$dir"; \
		python3 -m unittest discover -s "$$dir" || exit 1; \
	done

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

# `run` and `example` are gone: `otter start --integrations ./examples` is the
# same runtime with a free port, and it records where it listens.

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

## docker: build the runtime image (run make build first)
docker:
	docker build -t $(IMAGE):$(VERSION) .

# Stopping first is not politeness: $(DATA) holds both the extracted Python SDK
# the running daemon put on its children's PYTHONPATH and the SQLite database it
# holds open. Deleting them underneath a live daemon breaks `import otter`
# (ModuleNotFoundError) and unlinking the database silently discards its state.
# Stopping first is not politeness: ./tmp holds the extracted Python SDK the
# running daemon put on its children's PYTHONPATH and the SQLite file it holds
# open. Deleting them underneath a live daemon breaks `import otter` and
# silently discards state. `otter stop` stops the runtime serving this
# checkout, which is not necessarily the one on the default port.
## clean: remove ./bin and ./tmp (stops this checkout's runtime first)
clean:
	-@OTTER_SERVE_DIR="$(ROOT)/.otter/serve" $(OTTER) stop
	rm -rf $(ROOT)/$(BIN) $(ROOT)/$(DATA)

## fmt: rewrite all Go files with gofmt
fmt:
	gofmt -w $(GO_DIRS)

## tidy: sync go.mod/go.sum
tidy:
	$(GO) mod tidy
# ---------------------------------------------------------------------------
# Integration operations
#
# These are thin wrappers over `otter`, for one integration at a time, with the
# two paths that are easy to get wrong (integrations root, data directory)
# filled in. Everything they call works without make:
#
#   make release INTEGRATION=shopify-to-salesforce
#   make schema  INTEGRATION=... SYSTEM=salesforce OBJECT=Contact
#
# Everything else an operator does to a running runtime is already a command
# and is deliberately not duplicated here. The mapping, so nobody has to guess
# which of two names is current:
#
#   was                 is now
#   make sync-up        otter start
#   make sync-restart   otter stop && otter start
#   make sync-stop      otter stop
#   make sync-run       otter run <integration>
#   make sync-schedule  otter integrations --schedule
#   make sync-logs      otter logs <run-id> [--follow]
#   make sync-status    otter state get / otter runs
#   make sync-retry     otter state set / delete, then otter run
#   make run            otter start --integrations ./examples
#   make example        otter start --integrations ./examples --log-format=pretty
#
# The sync-* names were a second interface over the same runtime, and one of
# them (`sync-stop`) killed whatever held port 7337 rather than the runtime
# serving this checkout. A single interface is worth more than the muscle
# memory.
# ---------------------------------------------------------------------------

INTEGRATION  ?= shopify-to-salesforce
INTEGRATIONS ?= ./integrations

# The interpreter used for authoring tooling. The runtime never needs this: a
# managed integration prepares its own interpreter, and an external one uses
# whatever `python.executable` names.
PYTHON ?= python3

# Which vendor schema `make schema` pulls unless OBJECT overrides it.
SYSTEM ?= salesforce
OBJECT ?=

## release: stage, prepare and activate INTEGRATION's managed Python release
release:
	@$(OTTER) release --integrations $(INTEGRATIONS) --data $(DATA) --shared ../../lib $(INTEGRATION)

## release-list: staged releases for INTEGRATION, newest first
release-list:
	@$(OTTER) release --list --data $(DATA) $(INTEGRATION)

# The schema puller is integration *authoring* tooling: it writes a typed
# model of a vendor's API into the integration so mapping code can be checked
# against it. It is not part of the runtime, so it is not an `otter` command.
## schema: pull INTEGRATION's vendor schema (SYSTEM=, OBJECT= to override)
schema:
	@$(PYTHON) lib/python/otter_schema/pull.py \
	  --integration $(INTEGRATIONS)/$(INTEGRATION) \
	  --system $(SYSTEM) \
	  $(foreach obj,$(OBJECT),--object $(obj))

# ---------------------------------------------------------------------------
# Local runtime
#
# `otter start` needs nothing from make. These exist because `make start` is
# what people type in this repository, and because `make` can rebuild first.
# ---------------------------------------------------------------------------

## start: run this checkout's runtime in the foreground (Ctrl-C stops it)
start: build
	@$(OTTER) start --integrations $(INTEGRATIONS) --data $(DATA) --log-format=pretty

## start-detached: the same runtime in the background; `make stop` ends it
start-detached: build
	@$(OTTER) start --detach --integrations $(INTEGRATIONS) --data $(DATA)

## stop: stop the runtime serving this checkout
stop:
	@OTTER_SERVE_DIR="$(ROOT)/.otter/serve" $(OTTER) stop

## restart: stop, then start in the foreground
restart: stop
	@$(MAKE) --no-print-directory start


# ---------------------------------------------------------------------------
# Deployment
#
# `otter deploy` converges a remote Linux host onto a running Otter runtime
# over SSH. There is no cloud API here: the host is your choice, and anything
# you can ssh to will do. See docs/deploy.md.
#
#   make deploy HOST=root@203.0.113.10
#   make deploy HOST=droplet IDENTITY=~/.ssh/otter
#
# HOST may also be an alias from ~/.ssh/config, which is the easiest way to
# keep host names, users and keys out of shell history.
# ---------------------------------------------------------------------------

HOST     ?=
IDENTITY ?=

# The tunnel's local port. 7337 is the daemon's own default, so the deployed
# API looks exactly like a local one to every other `otter` command.
TUNNEL_PORT ?= 7337
OTTER_API   ?= http://127.0.0.1:$(TUNNEL_PORT)

## deploy: converge HOST onto the current checkout (HOST=user@host)
deploy:
	@test -n "$(HOST)" || { echo "set HOST, e.g. make deploy HOST=root@203.0.113.10"; exit 1; }
	@$(MAKE) --no-print-directory build
	@$(OTTER) deploy --host $(HOST) $(if $(IDENTITY),--identity $(IDENTITY))

## deploy-plan: show what `make deploy` would change, without touching HOST
deploy-plan:
	@test -n "$(HOST)" || { echo "set HOST, e.g. make deploy-plan HOST=droplet"; exit 1; }
	@$(OTTER) deploy --host $(HOST) $(if $(IDENTITY),--identity $(IDENTITY)) --dry-run

## deploy-status: what this checkout last deployed, and how to reach it
deploy-status:
	@$(OTTER) deploy --status

## deploy-tunnel: forward the remote API to localhost (Ctrl-C stops)
deploy-tunnel:
	@test -n "$(HOST)" || { echo "set HOST, e.g. make deploy-tunnel HOST=droplet"; exit 1; }
	@echo "tunnel: $(OTTER_API) -> $(HOST) -- Ctrl-C to stop"
	@ssh -N -L $(TUNNEL_PORT):127.0.0.1:$(TUNNEL_PORT) $(HOST)

## deploy-remote-runs: open a short-lived tunnel and list the remote runs
deploy-remote-runs:
	@test -n "$(HOST)" || { echo "set HOST, e.g. make deploy-remote-runs HOST=droplet"; exit 1; }
	@token="$$(python3 -c 'import json,sys;print(json.load(open("$(ROOT)/.otter/state.secret.json")).get("api_token",""))' 2>/dev/null)"; \
	test -n "$$token" || { echo "no token in .otter/state.secret.json -- deploy first"; exit 1; }; \
	ssh -f -N -L $(TUNNEL_PORT):127.0.0.1:$(TUNNEL_PORT) $(HOST); \
	pid="$$(pgrep -f "ssh -f -N -L $(TUNNEL_PORT):127.0.0.1:$(TUNNEL_PORT)" || true)"; \
	trap 'test -n "$$pid" && kill $$pid 2>/dev/null || true' EXIT; \
	sleep 1; \
	$(OTTER) --api $(OTTER_API) --token "$$token" runs --limit 10

## deploy-destroy: stop and remove the deployment, keeping the remote data
deploy-destroy:
	@test -n "$(HOST)" || { echo "set HOST, e.g. make deploy-destroy HOST=droplet"; exit 1; }
	@$(OTTER) deploy --host $(HOST) $(if $(IDENTITY),--identity $(IDENTITY)) --destroy --keep-data

## deploy-purge: destroy the deployment AND delete the remote data directory
deploy-purge:
	@test -n "$(HOST)" || { echo "set HOST, e.g. make deploy-purge HOST=droplet"; exit 1; }
	@echo "this deletes every sync watermark and all run history on $(HOST)"
	@$(OTTER) deploy --host $(HOST) $(if $(IDENTITY),--identity $(IDENTITY)) --destroy --yes
