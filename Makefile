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
.PHONY: sync-up sync-run sync-status sync-retry sync-logs sync-stop sync-restart sync-release sync-schedule sync-schema
.PHONY: deploy deploy-plan deploy-status deploy-tunnel deploy-remote-runs deploy-destroy deploy-purge

## help: list the available targets (default goal)
help:
	@echo "Otter build targets (VERSION=$(VERSION))"
	@echo ""
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed -e 's/^## //' | awk -F': ' '{ printf "  %-18s %s\n", $$1, $$2 }'
	@echo ""
	@echo "Integration operations take INTEGRATION=$(INTEGRATION) (any directory"
	@echo "under $(INTEGRATIONS)). Credentials are shared by every integration and"
	@echo "read from $(SHARED_ENV)."
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
# themselves; only `sync-up` loads the environment files, because the daemon is
# what reads the secrets.
# ---------------------------------------------------------------------------

INTEGRATION  ?= shopify-to-salesforce
INTEGRATIONS ?= ./integrations

# Credentials, shared by every integration. One file rather than one per
# integration: the daemon's environment is a single process environment, and an
# integration only receives the keys its own manifest declares, so per-
# integration files isolated nothing while making one rotation an N-file edit.
SHARED_ENV   ?= ./otter.env

# Daemon-wide settings (notification URL, log level, retention). Not committed,
# not per-integration: it configures the daemon, so it applies locally and on
# the host identically.
DAEMON_ENV   ?= ./otter.daemon.env
OTTER        ?= $(BIN)/otter
OTTERD       ?= $(BIN)/otterd
PYTHON       ?= python3
API          ?= http://127.0.0.1:7337
API_PORT     ?= 7337

# Objects to pull for `sync-schema`, space-separated. Empty means "whatever the
# manifest says", which is the common case; set it to pull several, or to
# retarget, without editing anything:
#   make sync-schema INTEGRATION=x OBJECT="Contact Shopify_Order__c"
OBJECT       ?=

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

## sync-release: stage, prepare and activate the integration's managed release
sync-release:
	@test -x $(OTTER) || { echo "missing $(OTTER) -- run 'make build' first"; exit 1; }
	@$(OTTER) release --integrations $(INTEGRATIONS) --data $(DATA) --shared ../../lib $(INTEGRATION)
	@$(OTTER) release --list --data $(DATA) $(INTEGRATION)

## sync-schema: pull the integration's Salesforce schema (OBJECT= to override)
sync-schema:
	@$(PYTHON) lib/python/otter_schema/pull.py \
	  --integration $(INTEGRATIONS)/$(INTEGRATION) \
	  $(foreach obj,$(OBJECT),--object $(obj))

## sync-up: start the daemon with its environment (Ctrl-C stops)
sync-up:
	@test -x $(OTTERD) || { echo "missing $(OTTERD) -- run 'make build' first"; exit 1; }
	@test -f $(SHARED_ENV) || { echo "missing $(SHARED_ENV)"; echo "  cp $(INTEGRATIONS)/$(INTEGRATION)/.env.example $(SHARED_ENV)"; exit 1; }
	@if [ -f "$(DAEMON_ENV)" ]; then echo "daemon env: $(DAEMON_ENV)"; else echo "daemon env: none ($(DAEMON_ENV) not present)"; fi
	@echo "shared env: $(SHARED_ENV)"
	@echo "otterd on $(INTEGRATIONS), data in $(DATA), API on $(API) -- Ctrl-C to stop"
	@set -a; \
	  test -f "$(DAEMON_ENV)" && . $(DAEMON_ENV); \
	  . $(SHARED_ENV); \
	  set +a; \
	  exec $(OTTERD) --integrations $(INTEGRATIONS) --data $(DATA) --listen $${OTTER_LISTEN:-127.0.0.1:$(API_PORT)} --log-format pretty

## sync-run: trigger the integration now, wait, and show what it did
sync-run:
	@test -x $(OTTER) || { echo "missing $(OTTER) -- run 'make build' first"; exit 1; }
	@$(run_and_report)

## sync-schedule: is it running on a schedule? cron, next run, last outcome
sync-schedule:
	@test -x $(OTTER) || { echo "missing $(OTTER) -- run 'make build' first"; exit 1; }
	@$(OTTER) --api $(API) integrations --schedule

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
	@test -x $(OTTER) || { echo "missing $(OTTER) -- run 'make build' first"; exit 1; }
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
