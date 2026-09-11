# Otter

> Otter is a lightweight runtime for running integration code anywhere.
>
> Write normal Python. Otter handles scheduling, retries, durable state, logs, and execution.

Otter is a single self-hosted binary that runs your Python integrations as
child processes. It finds them on disk, triggers them on a schedule or on
demand, keeps their state, retries their failures and records everything they
print — using nothing but a local SQLite file and a Python interpreter.

It is deliberately **not** a visual workflow builder, not an iPaaS, and not a
distributed workflow engine. Integration logic belongs in Python; Otter
provides the runtime primitives that make it reliable.

---

## Quickstart (about five minutes)

You need Go 1.22+ to build, and Python 3 on the machine that runs the
integrations. There are no cloud accounts, no servers to provision and no
external services to start.

```bash
git clone <repo> otter
cd otter
make build
```

Start the daemon on the bundled examples:

```bash
./bin/otterd --integrations ./examples --data ./tmp
```

In another terminal, list what was discovered:

```console
$ ./bin/otter integrations
counter
customer-sync
```

Trigger the `counter` integration by hand and look at what happened:

```console
$ ./bin/otter run counter
4f1c2a7e-2b1d-4f6a-9c3e-8a5b0d7e1f22

$ ./bin/otter logs 4f1c2a7e-2b1d-4f6a-9c3e-8a5b0d7e1f22
run queued (trigger manual)
run started (attempt 1 of 3, trigger manual)
Counter executed {"count":1,"level":"info"}
run succeeded (attempt 1, 148ms), exit code 0
```

The integration persisted a counter. Read it back:

```console
$ ./bin/otter state get counter count
1
```

Run it again and the state advances:

```console
$ ./bin/otter run counter
9a2f5c31-7d4e-4a8b-b6c1-2e3f4a5b6c7d
$ ./bin/otter state get counter count
2
```

Now restart the daemon (Ctrl-C, then start it again with the same `--data`
directory) and run it a third time:

```console
$ ./bin/otter state get counter count
3
```

That is the whole runtime: discovery, scheduling, execution, durable state,
logs and run history, surviving a restart. `make example` is the same thing
with human-readable daemon logs.

> `counter` is also registered on an hourly cron (`0 * * * *`), so it advances on
> its own too. It is deliberately not `* * * * *`: an every-minute schedule would
> race the `1`/`2`/`3` sequence above. `otter inspect counter` shows the registered
> expression and the next fire time.

---

## What an integration looks like

An integration is a directory with a manifest and a Python entrypoint:

```text
salesforce-to-netsuite/
├── otter.yaml
├── main.py
└── requirements.txt
```

```yaml
# otter.yaml
version: 1

name: salesforce-to-netsuite
entrypoint: main.py

trigger:
  cron: "*/5 * * * *"

timeout: 300
concurrency: 1

retry:
  attempts: 5
  backoff: exponential
  initial_delay: 2s
  max_delay: 60s

env:
  NETSUITE_ACCOUNT: ${NETSUITE_ACCOUNT}

secrets:
  - SALESFORCE_TOKEN
  - NETSUITE_TOKEN
```

```python
# main.py
from otter import run


@run
def main(ctx):
    cursor = ctx.state.get("cursor")
    records = get_changed_salesforce_accounts(cursor)
    for record in records:
        upsert_netsuite_customer(record)
    if records:
        ctx.state.set("cursor", records[-1]["last_modified"])
```

`@run` executes the function as soon as the module is loaded under Otter and
turns an exception into a non-zero exit code, which is what triggers the retry
policy. Without `@run`, build the context yourself — both styles are shown
below.

Point the daemon at the directory that contains it:

```bash
./bin/otterd --integrations /opt/otter/integrations --data /var/lib/otter
```

Every `otter.yaml` under that root is discovered recursively, validated and
registered. A broken manifest is reported and marked invalid — it never takes
the daemon down.

---

## The Python SDK

The SDK is tiny, dependency-free (standard library only) and ships **inside**
the `otterd` binary. On first start it is extracted to `<data>/sdk/python` and
prepended to the child's `PYTHONPATH`, so `import otter` works with no `pip
install` step.

Two supported styles:

```python
from otter import run

@run
def main(ctx):
    cursor = ctx.state.get("cursor")
    ctx.log.info("Starting sync", cursor=cursor)
    ctx.state.set("cursor", "123")
```

```python
from otter import Context

ctx = Context.from_environment()

count = ctx.state.get("count") or 0
count += 1
ctx.log.info("Counter executed", count=count)
ctx.state.set("count", count)
```

Available on the context:

| Member | Description |
| --- | --- |
| `ctx.state.get(key, default=None)` | Read a JSON value; `default` when unset. |
| `ctx.state.set(key, value)` | Persist any JSON-serialisable value. |
| `ctx.state.delete(key)` | Remove a key; returns `True` if it existed. |
| `ctx.state.all()` | Every key/value pair for this integration. |
| `ctx.log.debug/info/warning/error(message, **fields)` | Structured log line, stored against the run. |
| `ctx.run_id`, `ctx.integration_id` | Identifiers for the current run. |
| `ctx.trigger.type` | `manual`, `cron` or `webhook`. |
| `ctx.trigger.body`, `ctx.trigger.headers`, `ctx.trigger.header(name)` | The webhook payload, when there is one. |

The authoritative state lives in the daemon. The SDK is a thin HTTP client, so
a crashed or killed process can never corrupt it.

---

## Triggers

**Cron** — standard five-field expressions, re-registered from manifests on
every start, so a restart never loses a future schedule. Occurrences missed
while the daemon was offline are not replayed.

```yaml
trigger:
  cron: "*/5 * * * *"
```

**Manual** — always available, regardless of manifest configuration:

```bash
./bin/otter run salesforce-to-netsuite
```

**Webhook** — opt in per integration:

```yaml
trigger:
  webhook:
    enabled: true
```

Otter generates a static token on first start, persists it, and exports it
through the API:

```console
$ ./bin/otter inspect shopify-to-erp
webhook:       enabled
webhook url:   POST http://127.0.0.1:7337/v1/hooks/shopify-to-erp
webhook token: 5cde21dc53f207cb621b98df53d4caa06d1c15ffb98a084165f2094bdee757c1
               curl -X POST http://127.0.0.1:7337/v1/hooks/shopify-to-erp -H 'X-Otter-Token: ...' -d '{}'
```

Unauthenticated execution is never exposed: a webhook call without the token
gets a `401`.

---

## Reliability

These are the behaviours that make an integration safe to leave running.

**Timeouts.** `timeout: 300` sends `SIGTERM` to the process group, waits about
five seconds, then sends `SIGKILL`. The run is marked `timed_out` and the retry
policy applies.

**Retries.** `retry.attempts` is the *total* number of attempts, so
`attempts: 5` means one initial attempt plus up to four retries. Delays follow
`backoff` (`none`, `linear`, `exponential`) from `initial_delay`, capped by
`max_delay`. Configuration failures — a missing secret, a missing entrypoint, an
invalid manifest — are never retried, because the next attempt would fail
identically.

Each attempt is its own run record linked by `parent_run_id`, so the whole
chain is auditable:

```console
$ ./bin/otter run-status afd1b271-7e83-4bbc-a202-a7592fc0320d
run id:        afd1b271-7e83-4bbc-a202-a7592fc0320d
integration:   flaky
status:        failed
attempt:       1
error:         process exited with code 1

attempts:
  RUN ID                                ATTEMPT  STATUS     STARTED   DURATION  ERROR
  afd1b271-7e83-4bbc-a202-a7592fc0320d  1        failed     13:16:22  150ms     process exited with code 1
  b69229bd-96af-4c4e-a9cd-77d5190651a4  2        failed     13:16:23  120ms     process exited with code 1
  8a6c3c9e-8cbe-4a23-a287-1f7a6cd5fa18  3        succeeded  13:16:25  103ms     -
```

**Concurrency.** `concurrency: N` limits simultaneous runs of one integration;
extra triggers queue durably instead of piling up. `--workers N` caps total
concurrent runs (default: CPU cores, capped at 8).

**Crash recovery.** On start, any run left in `running` by a previous daemon is
marked `failed` with `otter daemon restarted during execution`, and a fresh
attempt is enqueued when the retry policy allows. Queued runs stay queued.
State, run history and logs survive.

**Graceful shutdown.** On `SIGTERM`/`SIGINT`, Otter stops the scheduler, stops
claiming work, gives running integrations `--shutdown-grace` (default 15s) to
finish, terminates what is left, records those runs as failed with `otter
daemon shut down during execution` (retry policy applies), and closes SQLite
cleanly.

**Checkpointing.** Because state is durable and written by the integration
itself, a crashed sync resumes instead of starting over. The bundled
`customer-sync` example demonstrates it: it stores its progress after *every*
customer, so a crash halfway through leaves the checkpoint exactly where it
stopped.

---

## Second example: a resumable sync

`examples/customer-sync` simulates a source API, a transformation and a
destination API using a local mock server — no SaaS credentials involved.

```bash
# terminal 1
python3 examples/customer-sync/mock_api.py

# terminal 2
./bin/otterd --integrations ./examples --data ./tmp
./bin/otter run customer-sync
./bin/otter state get customer-sync last_processed_customer_id
```

It pages through 25 customers five at a time and checkpoints after each one.
Run it again and it correctly does nothing:

```console
$ ./bin/otter logs $(./bin/otter run customer-sync) | tail -3
customer sync finished {"checkpoint":25,"level":"info","synced":0}
synced 0 customers; checkpoint=25
run succeeded (attempt 1, 82ms), exit code 0
```

Simulate a crash by starting the daemon with `CRASH_AFTER=3`. Each attempt
stops after three customers and the checkpoint advances to 3, then 6, then 9
across the retries instead of restarting:

```console
$ CRASH_AFTER=3 ./bin/otterd --integrations ./examples --data ./tmp/cs
$ ./bin/otter state get customer-sync last_processed_customer_id
9
```

Restart without `CRASH_AFTER` and the next run finishes the job — 16 more
customers, no duplicates:

```console
$ ./bin/otter logs $(./bin/otter run customer-sync) | tail -2
customer sync finished {"checkpoint":25,"level":"info","synced":16}
synced 16 customers; checkpoint=25
```

---

## CLI

```bash
otter status                            # daemon health, queue depth, run counts
otter integrations [--all]              # integration names (--all includes invalid)
otter inspect <integration>             # full manifest view, triggers, recent runs
otter run <integration> [--body <json>] # queue a manual run; prints the run id
otter runs [--integration I] [--status S] [--limit N]
otter run-status <run-id>               # one run plus its retry attempts
otter logs <run-id> [--follow]          # captured output
otter state get <integration> <key>
otter state set <integration> <key> <json>
otter state delete <integration> <key>
otter validate <otter.yaml|directory>   # validate without a running daemon
otter serve                             # run the daemon (same as otterd)
```

Global flags: `--api <url>`, `--token <token>`, `--json`, `--version`.
`otter integrations`, `otter run` and `otter state get` print machine-friendly
output so they compose in scripts:

```bash
otter logs "$(otter run counter)" --follow
```

---

## Configuration

```bash
otterd \
  --integrations /opt/otter/integrations \
  --data /var/lib/otter \
  --listen 127.0.0.1:7337 \
  --workers 8
```

| Flag | Environment | Default | Purpose |
| --- | --- | --- | --- |
| `--integrations` | `OTTER_INTEGRATIONS_DIR` | `./integrations` | Root scanned recursively for `otter.yaml`. |
| `--data` | `OTTER_DATA_DIR` | `./tmp` | Holds `otter.db` and the extracted SDK. |
| `--listen` | `OTTER_LISTEN` | `127.0.0.1:7337` | HTTP API address. |
| `--workers` | `OTTER_WORKERS` | CPU cores, max 8 | Maximum concurrent runs. |
| `--api-token` | `OTTER_API_TOKEN` | *(none)* | Bearer token; **required** for non-loopback binding. |
| `--log-format` | `OTTER_LOG_FORMAT` | `json` | `json` or `pretty`. |
| `--log-level` | `OTTER_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `--shutdown-grace` | `OTTER_SHUTDOWN_GRACE` | `15s` | Time running integrations get on shutdown. |
| `--sdk-path` | `OTTER_SDK_PATH` | *(embedded)* | Override the directory put on the child `PYTHONPATH`. |

The daemon logs structured JSON to stdout by default:

```json
{"event":"run_succeeded","exit_code":0,"integration":"counter","level":"info","run_id":"...","timestamp":"..."}
```

Use `--log-format pretty` during development.

### Secrets

Secrets are read from the **daemon's** environment and injected into the child
process:

```yaml
secrets:
  - SHOPIFY_TOKEN
```

If one is missing, the run fails before Python starts and is not retried:

```text
integration shopify-to-erp requires secrets that are not available: SHOPIFY_TOKEN
```

`env` values may reference the daemon environment or a resolved secret with
`${VAR}`. The daemon's own `OTTER_API_TOKEN` is stripped from the child
environment, and each child receives a short-lived token scoped to its own run
and integration only.

The internal `SecretProvider` interface is the seam for AWS Secrets Manager,
Vault, 1Password or a control plane later; today only the environment provider
is implemented.

---

## HTTP API

The daemon listens on `127.0.0.1:7337` by default.

```text
GET    /health
GET    /v1/integrations
GET    /v1/integrations/{id}
POST   /v1/integrations/{id}/runs
GET    /v1/runs                     ?integration_id=&status=&limit=&offset=
GET    /v1/runs/{id}
GET    /v1/runs/{id}/logs           ?after_id=&limit=
POST   /v1/runs/{id}/logs
POST   /v1/runs/{id}/cancel
GET    /v1/integrations/{id}/state
GET    /v1/integrations/{id}/state/{key}
PUT    /v1/integrations/{id}/state/{key}
DELETE /v1/integrations/{id}/state/{key}
POST   /v1/hooks/{integration}
```

Loopback binding needs no token. Binding elsewhere requires
`OTTER_API_TOKEN`, and the daemon refuses to start without it:

```bash
curl -H "Authorization: Bearer $OTTER_API_TOKEN" http://127.0.0.1:7337/v1/integrations
```

See [docs/api-reference.md](docs/api-reference.md) for request and response
bodies, error codes and a curl walkthrough.

---

## Deployment

The native binary is the primary deployment mechanism; Docker is optional
convenience packaging.

```bash
make cross      # linux/amd64, linux/arm64, darwin/amd64, darwin/arm64 in ./bin
make docker     # python:3.13-slim image with both binaries
```

A minimal systemd unit:

```ini
[Unit]
Description=Otter integration runtime
After=network.target

[Service]
User=otter
EnvironmentFile=/etc/otter/otter.env
ExecStart=/usr/local/bin/otterd --integrations /opt/otter/integrations --data /var/lib/otter
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
```

Runs on a Linux VM, EC2, Docker, an on-prem server, a customer VPC or a
Raspberry Pi. The runtime requires only the Otter binary, Python and the local
filesystem. [docs/operations.md](docs/operations.md) covers backups, log
retention, upgrades and troubleshooting.

---

## Security

> An Otter integration executes with the operating-system permissions of the
> Otter worker process. Otter does not sandbox integrations in the MVP.

Integrations are trusted code supplied by the operator. Run the daemon as a
dedicated unprivileged user, keep the API on loopback, and use containers,
dedicated users or separate machines when isolation is required. Details and a
hardening checklist are in [docs/security.md](docs/security.md).

---

## Repository layout

```text
otter/
├── cmd/
│   ├── otter/          CLI entrypoint
│   └── otterd/         daemon entrypoint
├── internal/
│   ├── api/            HTTP API server and CLI client
│   ├── cli/            command implementations
│   ├── config/         manifest parsing, validation, discovery
│   ├── daemon/         orchestration: workers, queue, retries, recovery
│   ├── database/       SQLite connection and migrations
│   ├── executor/       child process execution, timeout, cancellation
│   ├── logging/        structured daemon logs
│   ├── queue/          durable run queue and atomic claiming
│   ├── retry/          backoff calculation
│   ├── runs/           run history and captured logs
│   ├── scheduler/      cron registration
│   ├── secrets/        SecretProvider and the environment provider
│   └── state/          durable per-integration key/value state
├── migrations/         embedded SQL schema
├── sdk/python/otter/   the Python SDK (embedded into the daemon binary)
├── examples/
│   ├── counter/        cron + state + logs
│   └── customer-sync/  resumable sync against a local mock API
└── docs/
```

The runtime is a single process with a durable SQLite queue underneath it. Read
[docs/architecture.md](docs/architecture.md) for the subsystem diagram, the
schema and the run lifecycle — it is a ten-minute read by design.

---

## Development

```bash
make build     # ./bin/otterd and ./bin/otter
make test      # unit and integration tests
make lint      # gofmt, go vet, golangci-lint when installed
make example   # run the daemon against ./examples with pretty logs
make cross     # cross-compile for Linux and macOS
```

The integration tests start a real daemon, run real Python processes and
verify run status, logs, state, retries, timeouts, concurrency, crash recovery
and shutdown. The SDK has its own suite:

```bash
python3 -m unittest discover -s sdk/python/tests
```

---

## Documentation

| Document | Contents |
| --- | --- |
| [docs/architecture.md](docs/architecture.md) | Subsystems, schema, run lifecycle, design rationale. |
| [docs/manifest-reference.md](docs/manifest-reference.md) | Every `otter.yaml` field with defaults and validation rules. |
| [docs/api-reference.md](docs/api-reference.md) | Every endpoint, credential type and error code. |
| [docs/operations.md](docs/operations.md) | Deployment, systemd, backups, retention, upgrades, troubleshooting. |
| [docs/security.md](docs/security.md) | Trust model, tokens, secrets, hardening checklist. |
| [docs/examples.md](docs/examples.md) | Worked examples, including webhook and scheduled patterns. |

---

## What Otter is not

No visual editor, no DSL, no proprietary connectors, no DAG engine, no
distributed scheduler, no Kubernetes operator, no browser UI, no cloud control
plane, no multi-tenant SaaS, no object storage, no arbitrary-language support.

The test for adding a primitive is simple: does almost every reliable
integration need it? If it is specific to a vendor, a workflow or a UI, it
belongs outside the runtime.
