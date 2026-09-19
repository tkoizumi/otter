# Otter Architecture

Otter is a small runtime that turns a directory of Python scripts into a
scheduled, observable, retrying set of integrations. This document explains how
the pieces fit together. It should take about ten minutes to read.

- [In one paragraph](#in-one-paragraph)
- [How Python actually runs](#how-python-actually-runs)
- [Persistence in one picture](#persistence-in-one-picture)
- [The developer experience](#the-developer-experience)
- [Daemon overview](#daemon-overview)
- [Subsystems](#subsystems)
- [Storage layout and SQLite schema](#storage-layout-and-sqlite-schema)
- [Run lifecycle](#run-lifecycle)
- [Queue claim algorithm](#queue-claim-algorithm)
- [Execution, timeouts and cancellation](#execution-timeouts-and-cancellation)
- [Crash recovery](#crash-recovery)
- [Graceful shutdown](#graceful-shutdown)
- [Logging](#logging)
- [Why Go, why SQLite, why child processes](#why-go-why-sqlite-why-child-processes)
- [What is deliberately NOT here](#what-is-deliberately-not-here)

## In one paragraph

`otterd` is a single Go binary. It scans a root directory for `otter.yaml`
files, registers their triggers, and runs each triggered execution of an
integration as a **separate child Python process** (`python3 main.py`).
Everything durable — integration state, run history, captured logs, the pending
run queue, webhook tokens — lives in one SQLite database. There is no Postgres,
no Redis, no Kafka, no message broker, no external scheduler and no UI. One
process, one database file, one binary.

## How Python actually runs

**There is no VM, no container and no sandbox, and Otter never interprets
Python.** `otterd` is a Go program with no Python linkage at all: no `libpython`,
no cgo (`CGO_ENABLED=0`), no CPython compiled into the binary. It does not parse,
compile or execute a single line of Python. What it does is `execve` the same
interpreter you would have started by hand:

```go
exec.Command("python3", "main.py")   // literally this argv
  Dir           = the integration's directory inside its active release
  Env           = the daemon's environment, minus every OTTER_* variable,
                  plus the per-run values and the manifest's env and secrets
  Stdout/Stderr = pipes the daemon reads line by line
  Stdin         = /dev/null
  SysProcAttr   = Setpgid, so signals reach any grandchildren
```

So **one run is one fresh operating-system process**, and CPython does the
parsing and execution exactly as it would in a shell. Nothing is carried between
attempts except what is in SQLite.

```
   otterd  (one Go process)                  child  (real python3)
   ┌────────────────────────┐                ┌────────────────────────┐
   │ scheduler              │   execve()     │ 1 interpreter starts   │
   │ run queue              │ ─────────────▶ │ 2 imports `otter` from │
   │ workers                │  argv + env    │   PYTHONPATH           │
   │ retries                │                │ 3 runs your main(ctx)  │
   │ state · logs           │                │                        │
   │ SQLite (WAL)           │                │                        │
   │  HTTP API :7337        │ ◀───────────── │                        │
   │                        │  stdout/stderr │                        │
   │                        │  + exit status │                        │
   │                        │ ◀───────────── │ ctx.state / ctx.log    │
   │                        │  loopback HTTP │ (the SDK is a client)  │
   └────────────────────────┘                └────────────────────────┘
              ▲
              │  cron ticks · `otter run` · POST /v1/hooks/... · POST .../runs
```

| Question | Answer |
| --- | --- |
| Is there a language VM? | No. CPython is the interpreter, started as a normal process. |
| Is there an embedded interpreter? | No. The daemon has no Python dependency and no cgo. |
| Is there a sandbox? | No. The child runs as the daemon's OS user with its full permissions — see [security.md](security.md). |
| Container per run? | No. `execve`. No container runtime, no image pull, no warm pool. |
| Which Python? | An external integration uses whatever `python.executable` resolves to (default `python3`), on `PATH` or absolute. A managed one uses the interpreter of the environment prepared for its release. |
| Do runs share memory? | No. Separate processes; the only channels are the environment, the pipes and loopback HTTP. |
| Can an integration read stdin? | No, it is `/dev/null`. An integration cannot prompt. |
| What does a run cost to start? | One interpreter start per attempt: a trivial run is ~75–150 ms end to end (measured for the bundled `counter`), plus your own imports. |
| What breaks if it crashes? | Only the run. A segfault, an OOM kill or `os._exit()` in the child cannot take the daemon down. |

**Why the child makes HTTP calls back to the daemon.** Because it is a process
rather than a function call, it cannot reach the daemon's memory. So `ctx.state`
and `ctx.log` are HTTP requests to `OTTER_API_URL`, authenticated with a
short-lived bearer token (`OTTER_STATE_TOKEN`) scoped to that one run and
integration. That is also why the *daemon* is the authority on state rather than
the SDK: the daemon is the only thing holding the database. The practical
consequences are that state is durable even if the child is killed mid-write,
and that the child needs no database driver at all.

For the exact environment the child receives, and how timeouts and cancellation
signal it, see [Execution, timeouts and cancellation](#execution-timeouts-and-cancellation).

### What runs is the release, not the tree

The directory the child runs in is not the integration directory in your
checkout: it is the integration's copy inside its **active release**, an
immutable snapshot addressed by a digest of its contents. Every integration
needs one before it can run -- external and managed Python alike -- and
submission is where the binding happens: the digest is recorded on the run, so
activating a newer release cannot move a queued or retried attempt onto
different code.

`otter release` stages and activates a release (`otter deploy` does it on the
host before the daemon restarts). What a release pins beyond the code depends on
the mode: managed Python also binds a prepared interpreter and a locked
dependency set, while an external integration runs the interpreter its manifest
names.

## Persistence in one picture

Everything durable is **one SQLite file** in WAL mode at `<data dir>/otter.db`.
The child process never opens it — all reads and writes go through the daemon,
which is the single writer. That is why there is no lock contention to tune and
nothing to run alongside it.

```
   child process                daemon                     otter.db  (WAL)
   ─────────────                ──────                     ──────────────────
   ctx.state.set ─── HTTP ────▶ state manager ───────────▶ integration_state
   ctx.log.info  ─── HTTP ────▶ log manager ─────────────▶ run_logs
   stdout/stderr ─── pipes ───▶ log manager ─────────────▶ run_logs
   exit code ─────── exec ────▶ retry manager ───────────▶ runs + run_queue
   cron / CLI / webhook ──────▶ trigger manager ─────────▶ runs + run_queue
   `otter state set` ── HTTP ─▶ API ─────────────────────▶ integration_state
```

What survives what:

| Event | Integration state | Run history & logs | Pending queue |
| --- | --- | --- | --- |
| Daemon restart (SIGTERM) | intact | intact | intact |
| Daemon killed (SIGKILL) | intact | intact; the in-flight run is marked failed at next start and retried | intact |
| Machine reboot | intact | intact | intact |
| Power loss mid-write | consistent; the last few commits may be lost | same | same |

That last row is the documented trade-off of WAL with `synchronous=NORMAL`: the
database is never *corrupted*, but the most recent commit is not guaranteed to
have reached disk. It is a safe trade here because every write in a sync is
idempotent — at worst a run re-processes the page it was in the middle of, which
an upsert by external ID makes harmless. Nothing outside the data directory
holds any state, which is why a backup is just a file copy.

Schema details, the run lifecycle and the queue claim algorithm follow below.

## The developer experience

An integration is just a directory:

```
salesforce-to-netsuite/
├── otter.yaml        # manifest: name, entrypoint, trigger, retry, secrets
├── main.py           # plain Python; stdlib only is fine
└── requirements.txt  # optional, your own tooling installs it
```

The developer writes Python and a small YAML file. Otter supplies scheduling,
run history, retries, timeouts, log capture, durable key/value state and an
HTTP API. Nothing else is required — in particular, no `pip install otter`,
because the Python SDK ships inside the `otterd` binary and is extracted to the
data directory.

## Daemon overview

```
                          otterd (single Go process)
                          ┌────────────────────────────────────────────────┐
  otter.yaml files        │                                                │
  ┌──────────────┐        │  ┌────────────────┐    ┌──────────────────┐   │
  │ integrations/│──walk──┼─▶│ Config loader  │───▶│ Discovery /      │   │
  │  a/otter.yaml│        │  │ (parse+validate)│    │ registry         │   │
  │  b/otter.yaml│        │  └────────────────┘    └────────┬─────────┘   │
  └──────────────┘        │                                 │             │
                          │        ┌────────────────────────┼──────────┐  │
  cron ticks ─────────────┼───────▶│ Scheduler (cron)       │          │  │
  POST /v1/hooks/{id} ────┼───────▶│ Trigger manager        │          │  │
  POST /v1/.../runs ──────┼───────▶│ (manual, CLI, API)     │          │  │
  otter run <id> ─────────┼───────▶└───────────┬────────────┘          │  │
                          │                    │ enqueue(run)          │  │
                          │                    ▼                       │  │
                          │        ┌────────────────────────┐          │  │
                          │        │ Run queue (SQLite)     │          │  │
                          │        │ run_queue table        │          │  │
                          │        └───────────┬────────────┘          │  │
                          │                    │ atomic claim          │  │
                          │                    ▼                       │  │
                          │        ┌────────────────────────┐          │  │
                          │        │ Workers / Executor     │          │  │
                          │        │ --workers goroutines   │          │  │
                          │        └───┬────────────┬───────┘          │  │
                          │            │ spawn      │ capture          │  │
                          │            ▼            ▼                  │  │
                          │   python3 main.py   Log manager ──▶ run_logs│
                          │   (child process)        ▲                │  │
                          │            │ exit code   │                │  │
                          │            ▼             │                │  │
                          │   ┌────────────────┐     │                │  │
                          │   │ Retry manager  │─────┘                │  │
                          │   └───────┬────────┘                      │  │
                          │           │ new attempt (new run record)   │  │
                          │           └──────────────▶ run queue       │  │
                          │                                            │  │
                          │  ┌───────────────┐   ┌──────────────────┐  │  │
  SDK ────────────────────┼─▶│ State manager │   │ HTTP API         │  │  │
  (state get/set over     │  │ integration_  │◀──│ /health /v1/...  │  │  │
   HTTP with per-run      │  │ state table   │   │ bearer auth      │  │  │
   token)                 │  └───────────────┘   └────────┬─────────┘  │  │
                          │                               │            │  │
                          │                     ┌─────────▼─────────┐  │  │
                          │                     │ otter (CLI)       │  │  │
                          │                     └───────────────────┘  │  │
                          └────────────────────────────────────────────────┘
                                             │
                                    ┌────────▼────────┐
                                    │ SQLite (WAL)    │
                                    │ otter.db        │
                                    └─────────────────┘
```

Data flow in one line: **trigger → enqueue run → atomic claim → spawn child
process → capture logs → record terminal status → maybe enqueue retry.**

## Subsystems

| Subsystem | Responsibility |
| --- | --- |
| Config loader | Reads and strictly validates `otter.yaml`: schema version, name pattern, entrypoint containment, durations, retry policy, triggers, secrets list. Produces either a valid manifest or a structured validation error. |
| Discovery / registry | Walks the integrations root recursively looking for files named exactly `otter.yaml`, builds the integration registry, enforces unique `name`s, and keeps an entry (valid *or* invalid) for every manifest found. Re-runs on start, and on demand from the CLI/API. |
| Scheduler | Registers one cron entry per integration with `trigger.cron`, using the standard 5-field format. Fires `enqueue(run)` with `trigger_type=cron`. Cron occurrences missed while the daemon was offline are never replayed. |
| Trigger manager | Accepts external triggers: webhook HTTP requests, manual runs from the CLI/API, and scheduler callbacks. Normalizes all of them into a run enqueue with a `trigger_type`, `trigger_body` and `trigger_headers`. Runs scheduled manually work regardless of the manifest's trigger configuration. |
| Run queue | Durable FIFO in SQLite (`run_queue`). Enqueue inserts a row; the executor claims rows atomically. Because it is a table, queued work survives restarts and reboots. |
| Workers / executor | A fixed pool of `--workers` goroutines (default: number of CPU cores, capped at 8). Each worker claims queued runs, checks per-integration `concurrency`, spawns the child Python process with the right environment, captures stdout/stderr line by line, applies the timeout, records the exit code and the terminal status. |
| Retry manager | Decides, from the manifest's `retry` policy and the failure class, whether to schedule another attempt, computes the backoff delay (`none`, `linear`, `exponential`), and creates the next run record with `parent_run_id`/`root_run_id`/`attempt`. |
| State manager | Backs the SDK's `ctx.state` API and the `/v1/integrations/{id}/state` endpoints over the `integration_state` table. Values are arbitrary JSON; keys are validated against `[A-Za-z0-9._:-]{1,128}`. State belongs to the integration, not to a run, so it survives restarts. |
| Log manager | Writes captured child output into `run_logs` with a stream tag (`stdout`, `stderr`, `otter`), a monotonically increasing per-run sequence, and a timestamp. Serves `GET /v1/runs/{id}/logs` with `after_id`/`limit` for streaming. |
| HTTP API | Loopback-first JSON API (`127.0.0.1:7337` by default): health, integrations, manual runs, run status, logs, cancel, state, webhooks. Bearer auth for the control plane; per-run state tokens for child processes; per-integration webhook tokens for hooks. |
| CLI (`otter`) | Thin HTTP client over the same API. `otter serve` starts the daemon in the CLI process instead of talking to a remote one. |

## Storage layout and SQLite schema

```
<data dir>/
├── otter.db          # SQLite database (WAL mode)
├── otter.db-wal      # write-ahead log (transient; part of the database)
├── otter.db-shm      # shared-memory index (transient)
└── sdk/python/       # Python SDK extracted from the binary at startup
```

The daemon enables WAL mode so that reads (the API, the CLI) never block the
single writer (the executor recording runs and logs). The schema is versioned by
`schema_migrations`; on startup the daemon applies any migrations needed to move
an existing database to the current schema version.

The following is the schema the runtime expects. Column names are part of the
storage contract — the API and CLI never expose them directly, but operational
tooling (backups, retention jobs, troubleshooting) does.

### `integration_state`

One row per `(integration, key)`. The JSON value is stored as text.

| Column | Type | Notes |
| --- | --- | --- |
| `integration_id` | TEXT NOT NULL | Integration `name` from the manifest. |
| `key` | TEXT NOT NULL | Matches `[A-Za-z0-9._:-]{1,128}`. |
| `value` | TEXT | Arbitrary JSON, stored as its JSON text encoding. |
| `updated_at` | DATETIME NOT NULL | Last write. |
| PRIMARY KEY | (`integration_id`, `key`) | |

### `runs`

One row per **attempt**. A retry is a new row, not a mutation of the old one.

| Column | Type | Notes |
| --- | --- | --- |
| `id` | TEXT PRIMARY KEY | Run id, e.g. `run_01HZY...`; printed by `otter run`. |
| `integration_id` | TEXT NOT NULL | Integration `name`. |
| `trigger_type` | TEXT NOT NULL | `cron`, `webhook` or `manual`. |
| `status` | TEXT NOT NULL | `queued`, `running`, `succeeded`, `failed`, `retrying`, `cancelled`, `timed_out`. |
| `attempt` | INTEGER NOT NULL DEFAULT 1 | 1 for the first try, incremented on each retry. |
| `parent_run_id` | TEXT | Previous attempt in the chain (`NULL` for attempt 1). |
| `created_at` | DATETIME NOT NULL | When the run record was created (equivalently: queued). |
| `started_at` | DATETIME | When the child process was spawned. |
| `finished_at` | DATETIME | When a terminal status was recorded. |
| `exit_code` | INTEGER | Child exit code, `NULL` if no process ran or it was signalled before exiting. |
| `error` | TEXT | Failure reason for `failed`/`timed_out`/`cancelled`. |
| `metadata` | TEXT | JSON blob carrying the trigger payload (`body`, `headers`), the effective `timeout_seconds` and the scheduled time for cron runs. |

Indexes: `(integration_id, created_at DESC)` for `otter runs --integration`,
`(status)` for status filters, `(parent_run_id)` for retry-chain walks,
`(created_at DESC)` for the default newest-first listing.

`root_run_id` and the `attempts` array are **derived**, not stored: the API walks
up `parent_run_id` to the root and back down the chain. That keeps the schema
linear and cheap to write while still giving every attempt a full chain view.

### `run_logs`

Captured child output plus runtime annotations.

| Column | Type | Notes |
| --- | --- | --- |
| `id` | INTEGER PRIMARY KEY AUTOINCREMENT | Also the cursor for `?after_id=`. |
| `run_id` | TEXT NOT NULL | References `runs.id`. |
| `timestamp` | DATETIME NOT NULL | Capture time. |
| `stream` | TEXT NOT NULL | `stdout`, `stderr` or `otter` (runtime annotations: started, timed out, exit code). |
| `message` | TEXT NOT NULL | One line, newline stripped. |

Index: `(run_id, id)`.

### `run_queue`

Pending work. A row exists here only while a run is waiting to be executed;
claiming deletes it.

| Column | Type | Notes |
| --- | --- | --- |
| `run_id` | TEXT PRIMARY KEY | The `runs.id` to execute. The primary key makes double-enqueue impossible. |
| `integration_id` | TEXT NOT NULL | Denormalized for the per-integration concurrency check. |
| `available_at` | DATETIME NOT NULL | Earliest claim time; retry backoff is implemented by pushing this into the future. |
| `created_at` | DATETIME NOT NULL | When the run entered the queue, used as the FIFO tiebreaker. |

Index: `(available_at, created_at)`.

### `webhook_tokens`

Per-integration webhook credentials, generated on first start and persisted.

| Column | Type | Notes |
| --- | --- | --- |
| `integration_id` | TEXT PRIMARY KEY | Integration `name`. |
| `token` | TEXT NOT NULL | Sent by callers as `X-Otter-Token` or `?token=`. |
| `created_at` | DATETIME NOT NULL | Generation time. |

### `schema_migrations`

| Column | Type | Notes |
| --- | --- | --- |
| `version` | INTEGER PRIMARY KEY | Applied migration number. |
| `name` | TEXT NOT NULL | Human-readable migration name, from the migration filename. |
| `applied_at` | DATETIME NOT NULL | When it was applied. |

All timestamps are stored as fixed-width UTC strings
(`2006-01-02T15:04:05.000000000Z07:00`) so that lexicographic ordering matches
chronological ordering and `available_at <= ?` comparisons work in SQL without
any timezone ambiguity.

## Run lifecycle

```
                       enqueue (cron | webhook | manual)
                                     │
                                     ▼
                                ┌─────────┐
                                │ queued  │
                                └────┬────┘
                      atomic claim   │
                                     ▼
                                ┌─────────┐
     SIGTERM/Ctrl-C ───────────▶│ running │───────────────▶ succeeded (exit 0)
     POST .../cancel            └────┬────┘
                                     │
                    ┌────────────────┼─────────────────┐
                    │                │                 │
             non-zero exit      timeout fired     child process
                    │                │            disappeared
                    ▼                ▼                 │
              ┌──────────┐    ┌───────────┐           │
              │ retrying │    │ timed_out │           │
              └────┬─────┘    └─────┬─────┘           │
                   │                │                 │
        backoff    │         retries│exhausted        │
        elapsed    │                ▼                 ▼
                   │           ┌────────┐        ┌────────┐
                   └──▶ new    │ failed │        │ failed │
                        attempt└────────┘        └────────┘
                        (queued)
```

Terminal statuses never change again: `succeeded`, `failed`, `cancelled`,
`timed_out`. `queued`, `running` and `retrying` are non-terminal.

**What is retried:** a non-zero process exit, a timeout, and a run marked
`failed` by crash recovery or shutdown (subject to the manifest's retry policy).

**What is never retried** (configuration failures — retrying would repeat the
same mistake):

- cancelled runs,
- a missing or non-executable entrypoint,
- an invalid manifest,
- a secret listed in `secrets` that is missing from the daemon environment.

A run whose retry policy is exhausted ends as `failed` (`timed_out` stays
`timed_out` only if no retry is scheduled; once a retry is scheduled the attempt
is `retrying`).

Every attempt is visible through `GET /v1/runs/{id}`:

```json
{
  "id": "run_01HZY3",
  "root_run_id": "run_01HZY1",
  "parent_run_id": "run_01HZY2",
  "attempt": 3,
  "latest_status": "retrying",
  "attempts": [
    {"id": "run_01HZY1", "attempt": 1, "status": "failed",  "exit_code": 1},
    {"id": "run_01HZY2", "attempt": 2, "status": "failed",  "exit_code": 1},
    {"id": "run_01HZY3", "attempt": 3, "status": "retrying"}
  ]
}
```

## Queue claim algorithm

Claiming must be safe even though "is this integration at its concurrency
limit?" and "take the oldest eligible run" are two separate facts. Otter does
the whole claim inside **one immediate SQLite transaction**, so a run can never
be handed to two workers:

```text
BEGIN IMMEDIATE
  SELECT run_id, integration_id, available_at, created_at
    FROM run_queue
   WHERE available_at <= :now              -- backoff gate
   ORDER BY available_at ASC, created_at ASC, run_id ASC
   LIMIT 200                               -- candidate window
  FOR each candidate in order:
      if capacity.Reserve(candidate.integration_id):
          DELETE FROM run_queue WHERE run_id = candidate.run_id
          COMMIT; return candidate         -- slot reserved for this worker
  COMMIT; return ErrEmpty                  -- nothing claimable yet
```

Then, in the worker that won the claim, one more small transaction flips the
run's own record:

```sql
UPDATE runs SET status = 'running', started_at = :now WHERE id = :run_id;
```

The pieces that matter in practice:

- **Per-integration `concurrency`** is enforced by an in-memory capacity
  registry with an atomic `Reserve(integration_id) bool` / `Release(...)` pair,
  seeded from the manifests at startup. The reservation happens *inside* the
  claim transaction, so "this integration has room" and "this worker takes that
  run" cannot be decided separately. A worker that cannot reserve a slot moves
  on to the next candidate instead of blocking.
- **Global `--workers`** is enforced twice over: there are exactly `N` worker
  goroutines, and the same capacity registry refuses reservations once `N` runs
  are executing. Default is the number of CPU cores, capped at 8.
- **After a restart**, the queue is reloaded from `run_queue` while the capacity
  counters start empty. That is correct because no child processes survived the
  restart: crash recovery has already marked every previously `running` run as
  `failed`, so nothing is executing when the first worker reserves a slot.
- **Retry backoff** is implemented with `available_at`. A retry is enqueued
  immediately with a future `available_at`, so the claim query ignores it until
  the delay elapses. The timer needs no in-memory bookkeeping and survives a
  restart — a retry that was waiting out its backoff when the daemon stopped is
  simply claimable once the clock passes its `available_at`.
- **Ordering** is `available_at`, then `created_at`, then `run_id`: eligible
  work is FIFO, and retries rejoin the line when their backoff expires rather
  than jumping it.
- **Safety.** `run_queue.run_id` is the primary key, so a double-enqueue is
  impossible, and the immediate transaction serializes claims across workers.
- If the claim returns `ErrEmpty`, the worker sleeps briefly and retries.
  Because the queue is a table, "nothing to do" and "daemon restarted" are the
  same code path.

## Execution, timeouts and cancellation

Each run is a separate child process, so a crashing, hanging, memory-hungry or
`os._exit()`-ing integration cannot take the daemon down.

The child is started as `python.executable` (default `python3`) with
`main.py`'s directory as its working directory and this environment:

| Variable | Value |
| --- | --- |
| `OTTER_INTEGRATION_ID` | Integration `name`. |
| `OTTER_RUN_ID` | This attempt's run id. |
| `OTTER_API_URL` | Base URL of the daemon API (e.g. `http://127.0.0.1:7337`). |
| `OTTER_STATE_TOKEN` | Short-lived token authorizing state and log calls for this run. |
| `OTTER_TRIGGER_TYPE` | `cron`, `webhook` or `manual`. |
| `OTTER_INTEGRATION_DIR` | Absolute path of the integration directory. |

plus every entry from `env` (with `${VAR}` expanded against the daemon's
environment) and every name listed in `secrets` (read from the daemon's
environment). Every inherited `OTTER_*` variable is **stripped** before the child
environment is built, so the daemon's own `OTTER_API_TOKEN` can never leak into
integration code; the child sees only the scoped, per-run values above.
`PYTHONUNBUFFERED=1` and `PYTHONDONTWRITEBYTECODE=1` are set so log lines arrive
as they are written and integrations never need write access to their own
directory. `<data dir>/sdk/python` — or `--sdk-path` if set — is prepended to
`PYTHONPATH`, so `from otter import run` works with no installation step.

stdout and stderr are read line by line and written to `run_logs` as they
arrive, tagged with their stream; runtime annotations use the `otter` stream.
The exit code and `started_at`/`finished_at` are recorded on the run.

**Timeout** (`timeout`, default 300s): on expiry the executor sends `SIGTERM` to
the child's process group, waits about 5 seconds for it to exit, then sends
`SIGKILL` to the group. The run is marked `timed_out`, and the retry policy
applies. Sending the signal to the process group matters: a naive
`subprocess.run()` in an integration that has spawned its own children would
otherwise leave them behind. (This 5-second terminate grace is separate from
`--shutdown-grace`, which is how long the daemon waits for runs to finish on its
own before it starts terminating them at all.)

**Cancellation**: `POST /v1/runs/{id}/cancel`, or Ctrl-C on a foreground
`otterd`. Cancellation uses the same SIGTERM → grace → SIGKILL sequence, marks
the run `cancelled`, and the run is **not** retried.

## Crash recovery

SQLite is the only source of truth, so recovery after `kill -9`, a power loss or
an OOM kill is deterministic. On startup the daemon, before it starts claiming
work:

1. Runs schema migrations.
2. Finds every run still in `running` from the previous process lifetime. Those
   child processes are gone (the daemon was their parent), so each is marked
   `failed` with the error `otter daemon restarted during execution`.
3. Enqueues a retry for each of those runs when the manifest's retry policy
   permits it.
4. Leaves `queued` runs queued — they simply get claimed by the new worker pool.
5. Registers cron schedules from the manifests again. Occurrences that fell in
   the downtime window are **not** replayed. If a schedule needs catch-up
   semantics, model it as state (for example a `last_processed_at` checkpoint)
   so the next run reconciles the gap, as `examples/customer-sync` does.

Integration state, run history, logs and webhook tokens all survive restarts and
reboots because they are rows in `otter.db`, not memory.

## Graceful shutdown

On `SIGTERM` (systemd stop, container stop) or `SIGINT` (Ctrl-C), `otterd`:

1. Stops the scheduler so no new cron occurrences fire.
2. Stops claiming queued runs, so the queue drains to zero workers in flight.
3. Gives every running integration up to `--shutdown-grace` (default `15s`) to
   finish on its own.
4. Terminates whatever is still running (SIGTERM to the process group, then
   SIGKILL after ~5s) and marks those runs `failed` with the error
   `otter daemon shut down during execution`. Those runs **do** follow the retry
   policy, so they are re-enqueued on the next start.
5. Closes SQLite cleanly (checkpointing the WAL) and exits 0.

Set `--shutdown-grace` to your longest expected run if you would rather wait
than retry; the default trades a slower restart for a fast, predictable one.

## Logging

Daemon logs go to stdout, never to a file, so the supervisor (systemd/journald)
owns rotation.

- Default format is structured JSON, one object per line:

  ```json
  {"level":"info","event":"run_started","integration":"shopify-to-erp","run_id":"run_01HZY3","timestamp":"2024-06-01T12:00:03Z"}
  ```

- `--log-format pretty` switches to human-readable lines for local development.
- `--log-level debug|info|warn|error` (default `info`) filters output.
- Access logs for the HTTP API are emitted as `event: "http_request"` records.

Integration output is a separate concern: it is captured into the `run_logs`
table and read back with `otter logs <run-id>` / `GET /v1/runs/{id}/logs`. That
data grows without bound unless you prune it — see the retention example in
[operations.md](operations.md).

## Why Go, why SQLite, why child processes

**Why Go.** One static binary with no runtime dependency can be scp'd to any
Linux server and started; `CGO_ENABLED=0` cross-compiles to `linux/amd64`,
`linux/arm64` and macOS from one machine. Goroutines make "N workers, each
supervising a child process" straightforward, and `os/exec` gives process-group
signalling and timeout handling without a helper library. A single process is
also the simplest thing to supervise.

**Why SQLite.** A self-hosted runtime must not require an operator to run
Postgres and Redis before their first integration works. SQLite is embedded, has
no network hop, is transactional, and in WAL mode handles "one writer, many
readers" perfectly for this workload. The pure-Go driver means no cgo and no
libsqlite3 on the target host. Durability is a file-level property: back up one
file, and state, history and queue all move with it.

**Why child processes (rather than an in-process Python embed or a
container-per-run).** Child processes give the three properties that matter
most: a hard crash boundary (an integration that segfaults or calls
`os._exit()` cannot take `otterd` down), a hard timeout (`SIGKILL` a process
group); and an ordinary developer workflow (run `python3 main.py` by hand,
debug it normally, no embedded interpreter to match versions with). Containers
per run would add a container runtime dependency and a lot of startup latency;
this design keeps a Raspberry Pi viable as a target.

## What is deliberately NOT here

- **No DAG engine.** Integrations are independent units. If you need
  orchestration, sequence it inside one Python program or trigger the next
  integration over the API.
- **No distributed consensus / multi-node clustering.** One daemon owns one
  SQLite file. Run a second daemon with its own data directory if you need a
  second host.
- **No connectors.** There is no Salesforce/Shopify/NetSuite adapter layer, by
  design. The integration *is* the connector, written in Python with whatever
  HTTP client the developer likes.
- **No UI.** The CLI and the HTTP API are the interfaces; JSON out means you can
  pipe it anywhere.
- **No plugin system, no object storage, no external secrets backend (yet).**
  The `SecretProvider` interface is designed to grow these, but today secrets
  are environment variables on the daemon.
- **No broker.** No Kafka, no Redis, no RabbitMQ. The queue is a table.
