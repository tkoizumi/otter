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

.PHONY: help build test test-go test-python lint run example cross docker clean fmt tidy clean-pycache
.PHONY: sync-up sync-run sync-status sync-retry sync-logs sync-stop sync-restart

## help: list the available targets (default goal)
help:
	@echo "Otter build targets (VERSION=$(VERSION))"
	@echo ""
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed -e 's/^## //' | awk -F': ' '{ printf "  %-14s %s\n", $$1, $$2 }'
	@echo ""
	@echo "Integration operations take INTEGRATION=$(INTEGRATION) (any directory"
	@echo "under $(INTEGRATIONS)) and use $(ENV_FILE) for its secrets."
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

## docker: build the runtime image (run make build first)
docker:
	docker build -t $(IMAGE):$(VERSION) .

# Stopping first is not politeness: $(DATA) holds both the extracted Python SDK
# the running daemon put on its children's PYTHONPATH and the SQLite database it
# holds open. Deleting them underneath a live daemon breaks `import otter`
# (ModuleNotFoundError) and unlinking the database silently discards its state.
## clean: remove ./bin and ./tmp (stops the daemon first)
clean: sync-stop
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
# Deliberately parameterised rather than hard-coded to one integration: point
# INTEGRATION at any directory under INTEGRATIONS and the same targets work.
# Defaults target the shipped Shopify -> Salesforce sync.
#
# These talk to a *running* daemon over its HTTP API, so they need no secrets
# themselves; only `sync-up` loads the .env, because the daemon is what reads
# the secrets.
# ---------------------------------------------------------------------------

INTEGRATION  ?= shopify-to-salesforce
INTEGRATIONS ?= ./integrations
ENV_FILE     ?= $(INTEGRATIONS)/$(INTEGRATION)/.env
OTTER        ?= $(BIN)/otter
OTTERD       ?= $(BIN)/otterd
API          ?= http://127.0.0.1:7337
API_PORT     ?= 7337

# Where `sync-retry` rewinds the watermark to. Defaults to the example
# manifest's BACKFILL_FROM; raise it if your data starts later.
REWIND_TO ?= 2026-01-01T00:00:00Z

# Wait for a run to leave a non-terminal state, then print what it did.
define run_and_report
	id=$$($(OTTER) --api $(API) run $(INTEGRATION)) || exit 1; \
	echo "run: $$id"; \
	state=""; \
	for i in $$(seq 1 240); do \
		state=$$($(OTTER) --api $(API) --json run-status $$id 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin).get("status",""))' 2>/dev/null); \
		case "$$state" in queued|running|retrying|"") sleep 0.5 ;; *) break ;; esac; \
	done; \
	echo "status: $$state"; \
	$(OTTER) --api $(API) logs $$id | grep -E 'sync finished|rejected|integration failed' || true
endef

# Print the sync's durable state plus recent runs.
define print_status
	echo "--- state ---"; \
	for key in sync_cursor failed_total; do \
		printf '%-18s' "$$key:"; $(OTTER) --api $(API) state get $(INTEGRATION) $$key 2>/dev/null || echo "unset"; \
	done; \
	printf '%-18s' "last_run:"; $(OTTER) --api $(API) state get $(INTEGRATION) last_run 2>/dev/null || echo "unset"; \
	printf '%-18s' "failed_customers:"; $(OTTER) --api $(API) state get $(INTEGRATION) failed_customers 2>/dev/null || echo "none"; \
	echo; echo "--- recent runs ---"; \
	$(OTTER) --api $(API) runs --integration $(INTEGRATION) --limit 5
endef

## sync-up: start the daemon with the integration's .env (Ctrl-C stops)
sync-up:
	@test -x $(OTTERD) || { echo "missing $(OTTERD) -- run 'make build' first"; exit 1; }
	@test -f $(ENV_FILE) || { echo "missing $(ENV_FILE)"; echo "  cp $(INTEGRATIONS)/$(INTEGRATION)/.env.example $(ENV_FILE)"; exit 1; }
	@echo "otterd on $(INTEGRATIONS), data in $(DATA), API on $(API) -- Ctrl-C to stop"
	@set -a; . $(ENV_FILE); set +a; exec $(OTTERD) --integrations $(INTEGRATIONS) --data $(DATA) --listen $${OTTER_LISTEN:-127.0.0.1:$(API_PORT)} --log-format pretty

## sync-run: trigger the integration now, wait, and show what it did
sync-run:
	@test -x $(OTTER) || { echo "missing $(OTTER) -- run 'make build' first"; exit 1; }
	@$(run_and_report)

## sync-status: watermark, dead letters and recent runs
sync-status:
	@$(print_status)

## sync-retry: rewind the watermark to REWIND_TO, clear dead letters, run, report
sync-retry:
	@echo "rewinding sync_cursor to $(REWIND_TO)"
	@$(OTTER) --api $(API) state set $(INTEGRATION) sync_cursor '"$(REWIND_TO)"' >/dev/null
	@$(OTTER) --api $(API) state delete $(INTEGRATION) in_progress_cursor >/dev/null 2>&1 || true
	@$(OTTER) --api $(API) state delete $(INTEGRATION) in_progress_window_start >/dev/null 2>&1 || true
	@$(OTTER) --api $(API) state delete $(INTEGRATION) failed_customers >/dev/null 2>&1 || true
	@$(OTTER) --api $(API) state delete $(INTEGRATION) failed_total >/dev/null 2>&1 || true
	@$(run_and_report)
	@echo
	@$(print_status)

## sync-logs: logs for the latest run, or RUN=<id> for a specific one
sync-logs:
	@id="$(RUN)"; \
	if [ -z "$$id" ]; then \
		id=$$($(OTTER) --api $(API) runs --integration $(INTEGRATION) --limit 1 --json | python3 -c 'import sys,json;print(json.load(sys.stdin)[0]["id"])'); \
	fi; \
	echo "--- logs for $$id ---"; \
	$(OTTER) --api $(API) logs $$id

## sync-stop: stop whatever daemon is holding the API port
sync-stop:
	@pids="$$(lsof -ti:$(API_PORT) 2>/dev/null || true)"; \
	if [ -n "$$pids" ]; then echo "stopping daemon (pid $$pids)"; echo "$$pids" | xargs kill; \
	else echo "no daemon on port $(API_PORT)"; fi

## sync-restart: stop the daemon, then start it again with a fresh .env
sync-restart: sync-stop
	@sleep 1
	@$(MAKE) --no-print-directory sync-up
