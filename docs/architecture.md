# Otter Architecture

Otter is a small runtime that turns a directory of Python scripts into a
scheduled, observable, retrying set of jobs. This document explains how
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
- [Run timeline](#run-timeline)
- [Why Go, why SQLite, why child processes](#why-go-why-sqlite-why-child-processes)
- [What is deliberately NOT here](#what-is-deliberately-not-here)

This document explains how the runtime is built and why. It is descriptive. The
normative promises — the attempt state machine, what an accepted request
guarantees, delivery and state-concurrency limits, and the platform-specific
behavior — live in
[runtime-contract.md](runtime-contract.md), where every guarantee is tied to a
named fault-matrix scenario.

## In one paragraph

`otterd` is a single Go binary. It scans a root directory for `otter.yaml`
files, registers their triggers, and runs each triggered execution of an
job as a **separate child Python process** (`python3 main.py`).
Everything durable — job state, run history, captured logs, the pending
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
  Dir           = the job's directory inside its active release
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
| Which Python? | An external job uses whatever `python.executable` resolves to (default `python3`), on `PATH` or absolute. A managed one uses the interpreter of the environment prepared for its release. |
| Do runs share memory? | No. Separate processes; the only channels are the environment, the pipes and loopback HTTP. |
| Can a job read stdin? | No, it is `/dev/null`. A job cannot prompt. |
| What does a run cost to start? | One interpreter start per attempt: a trivial run is ~75–150 ms end to end (measured for the bundled `counter`), plus your own imports. |
| What breaks if it crashes? | Only the run. A segfault, an OOM kill or `os._exit()` in the child cannot take the daemon down. |

**Why the child makes HTTP calls back to the daemon.** Because it is a process
rather than a function call, it cannot reach the daemon's memory. So `ctx.state`
and `ctx.log` are HTTP requests to `OTTER_API_URL`, authenticated with a
short-lived bearer token (`OTTER_STATE_TOKEN`) scoped to that one run and
job. That is also why the *daemon* is the authority on state rather than
the SDK: the daemon is the only thing holding the database. The practical
consequences are that state is durable even if the child is killed mid-write,
and that the child needs no database driver at all.

For the exact environment the child receives, and how timeouts and cancellation
signal it, see [Execution, timeouts and cancellation](#execution-timeouts-and-cancellation).

### What runs is the release, not the tree

The directory the child runs in is not the job directory in your
checkout: it is the job's copy inside its **active release**, an
immutable snapshot addressed by a digest of its contents. Every job
needs one before it can run -- external and managed Python alike -- and
submission is where the binding happens: the digest is recorded on the run, so
activating a newer release cannot move a queued or retried attempt onto
different code.

`otter release` stages and activates a release (`otter deploy` does it on the
host before the daemon restarts). What a release pins beyond the code depends on
the mode: managed Python also binds a prepared interpreter and a locked
dependency set, while an external job runs the interpreter its manifest
names. The mode and the environment binding are read from the **bound release's**
manifest at submission, not from the live tree, so editing the source cannot
change how an already-staged release executes.

A release mirrors the workspace with one placement rule: the base is the
closest common ancestor of the jobs discovery root, the job
directory and every captured shared tree, and the job and each tree land
at their path relative to that base. The release root plays the part of the
base, so a manifest's relative `python.path` resolves verbatim. The discovery
root takes part because deploy releases with
`--jobs /opt/otter/jobs` while shared code lives at
`/opt/otter/lib/python`; without it the tree would have to be placed at
`../lib/python`, which no release path can express. The normalized placement and
each tree's destination name are part of the release digest, so a snapshot laid
out differently is a different release; the digest is prefixed with a version so
an older layout can never be reused. A release that cannot capture a declared
tree — missing, absolute, or reachable only through an escaping symlink — is
refused rather than shipped.

## Persistence in one picture

Everything durable is **one SQLite file** in WAL mode at `<data dir>/otter.db`.
The child process never opens it — all reads and writes go through the daemon,
which is the single writer. That is why there is no lock contention to tune and
nothing to run alongside it.

```
   child process                daemon                     otter.db  (WAL)
   ─────────────                ──────                     ──────────────────
   ctx.state.set ─── HTTP ────▶ state manager ───────────▶ job_state
   ctx.log.info  ─── HTTP ────▶ log manager ─────────────▶ run_logs
   stdout/stderr ─── pipes ───▶ log manager ─────────────▶ run_logs
   exit code ─────── exec ────▶ retry manager ───────────▶ runs + run_queue
   cron / CLI / webhook ──────▶ trigger manager ─────────▶ runs + run_queue
   `otter state set` ── HTTP ─▶ API ─────────────────────▶ job_state
```

What survives what:

| Event | Job state | Run history & logs | Pending queue |
| --- | --- | --- | --- |
| Daemon restart (SIGTERM) | intact | intact | intact |
| Daemon killed (SIGKILL) | intact | intact; the in-flight run is marked failed at next start and retried † | intact |
| Machine reboot | intact | intact | intact |
| Power loss mid-write | consistent; the last few commits may be lost | same | same |

† The SIGKILL row is the one platform-specific claim in this table. On Linux the
in-flight child is killed by the parent-death signal, so nothing survives to
overlap its retry; on macOS it is reparented and keeps running. See
[Child lifetime is platform-specific](#child-lifetime-is-platform-specific).

That last row is the documented trade-off of WAL with `synchronous=NORMAL`: the
database is never *corrupted*, but the most recent commit is not guaranteed to
have reached disk. It is a safe trade here because every write in a sync is
idempotent — at worst a run re-processes the page it was in the middle of, which
an upsert by external ID makes harmless. Nothing outside the data directory
holds any state, which is why a backup is just a file copy.

Schema details, the run lifecycle and the queue claim algorithm follow below.

## The developer experience

A job is just a directory:

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
  │ jobs/│──walk──┼─▶│ Config loader  │───▶│ Discovery /      │   │
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
  (state get/set over     │  │ job_  │◀──│ /health /v1/...  │  │  │
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
process → capture logs → record the terminal status and any successor attempt in
one transaction → notify.**

### Reload

The registry is read on startup and again on every `POST /v1/reload`. Reload is
not a restart: the process, the API listener, the worker pool and every
executing child are left alone. Only three things are replaced — the registry,
the per-job concurrency limits, and the cron triggers.

Discovery and webhook-token resolution run *before* any shared state is touched,
so walking a large jobs directory is invisible to everything already
running, and a failed walk leaves the previous set intact rather than
half-applied. `registry.load` then swaps the whole map under its write lock, so
a reader sees either the old set or the new one and never a torn intermediate
state.

Cron is reconciled by *diff*: `Scheduler.Replace` returns early when an
job's expression is unchanged. That matters because the cron runner
computes an entry's next fire time when the entry is added, so re-adding an
unchanged schedule would move that time and could skip an occurrence. An
job that did not change therefore keeps its schedule across a reload,
and the runner is never stopped.

Reload is the unit tier; a restart is the host tier. Changing the binary, the
embedded SDK or daemon-level configuration still replaces the process, because
those are process-scoped — see [Graceful shutdown](#graceful-shutdown).

## Subsystems

| Subsystem | Responsibility |
| --- | --- |
| Config loader | Reads and strictly validates `otter.yaml`: schema version, name pattern, entrypoint containment, durations, retry policy, triggers, secrets list. Produces either a valid manifest or a structured validation error. |
| Discovery / registry | Walks the jobs root recursively looking for files named exactly `otter.yaml`, builds the job registry, enforces unique `name`s, and keeps an entry (valid *or* invalid) for every manifest found. Re-runs on start, and on demand from the CLI/API. |
| Scheduler | Registers one cron entry per job with `trigger.cron`, using the standard 5-field format. Fires `enqueue(run)` with `trigger_type=cron`. Cron occurrences missed while the daemon was offline are never replayed. |
| Trigger manager | Accepts external triggers: webhook HTTP requests, manual runs from the CLI/API, and scheduler callbacks. Normalizes all of them into a run enqueue with a `trigger_type`, `trigger_body` and `trigger_headers`. Runs scheduled manually work regardless of the manifest's trigger configuration. |
| Run queue | Durable FIFO in SQLite (`run_queue`). Enqueue inserts a row; the executor claims rows atomically. Because it is a table, queued work survives restarts and reboots. |
| Workers / executor | A fixed pool of `--workers` goroutines (default: number of CPU cores, capped at 8). Each worker claims queued runs, checks per-job `concurrency`, spawns the child Python process with the right environment, captures stdout/stderr line by line, applies the timeout, records the exit code and the terminal status. |
| Retry manager | Decides, from the manifest's `retry` policy and the failure class, whether to schedule another attempt, computes the backoff delay (`none`, `linear`, `exponential`), and creates the next run record with `parent_run_id`/`root_run_id`/`attempt`. |
| State manager | Backs the SDK's `ctx.state` API and the `/v1/jobs/{id}/state` endpoints over the `job_state` table. Values are arbitrary JSON; keys are validated against `[A-Za-z0-9._:-]{1,128}`. State belongs to the job, not to a run, so it survives restarts. |
| Log manager | Writes captured child output into `run_logs` with a stream tag (`stdout`, `stderr`, `otter`), a monotonically increasing per-run sequence, and a timestamp. Serves `GET /v1/runs/{id}/logs` with `after_id`/`limit` for streaming. |
| HTTP API | Loopback-first JSON API (`127.0.0.1:7337` by default): health, jobs, manual runs, run status, logs, cancel, state, webhooks. Bearer auth for the control plane; per-run state tokens for child processes; per-job webhook tokens for hooks. |
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

### `job_state`

One row per `(job, key)`. The JSON value is stored as text.

| Column | Type | Notes |
| --- | --- | --- |
| `job_id` | TEXT NOT NULL | Job `name` from the manifest. |
| `key` | TEXT NOT NULL | Matches `[A-Za-z0-9._:-]{1,128}`. |
| `value` | TEXT | Arbitrary JSON, stored as its JSON text encoding. |
| `updated_at` | DATETIME NOT NULL | Last write. |
| PRIMARY KEY | (`job_id`, `key`) | |

### `runs`

One row per **attempt**. A retry is a new row, not a mutation of the old one.

| Column | Type | Notes |
| --- | --- | --- |
| `id` | TEXT PRIMARY KEY | Run id, e.g. `run_01HZY...`; printed by `otter run`. |
| `job_id` | TEXT NOT NULL | Job `name`. |
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

Indexes: `(job_id, created_at DESC)` for `otter runs <job>`,
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
| `job_id` | TEXT NOT NULL | Denormalized for the per-job concurrency check. |
| `available_at` | DATETIME NOT NULL | Earliest claim time; retry backoff is implemented by pushing this into the future. |
| `created_at` | DATETIME NOT NULL | When the run entered the queue, used as the FIFO tiebreaker. |

Index: `(available_at, created_at)`.

### `webhook_tokens`

Per-job webhook credentials, generated on first start and persisted.

| Column | Type | Notes |
| --- | --- | --- |
| `job_id` | TEXT PRIMARY KEY | Job `name`. |
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

The normative transition table, including which transitions are legal and how
each is proven, is in
[runtime-contract.md](runtime-contract.md#1-the-attempt-state-machine). What
follows is the shape of the lifecycle.

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
  "max_attempts": 5,
  "attempts": [
    {"id": "run_01HZY1", "attempt": 1, "status": "failed",  "exit_code": 1},
    {"id": "run_01HZY2", "attempt": 2, "status": "failed",  "exit_code": 1},
    {"id": "run_01HZY3", "attempt": 3, "status": "retrying"}
  ]
}
```

## Queue claim algorithm

Claiming must be safe even though "is this job at its concurrency
limit?" and "take the oldest eligible run" are two separate facts. Otter does
the whole claim inside **one immediate SQLite transaction**, so a run can never
be handed to two workers:

```text
BEGIN IMMEDIATE
  SELECT run_id, job_id, available_at, created_at
    FROM run_queue
   WHERE available_at <= :now              -- backoff gate
   ORDER BY available_at ASC, created_at ASC, run_id ASC
   LIMIT 200                               -- candidate window
  FOR each candidate in order:
      if capacity.Reserve(candidate.job_id):
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

- **Per-job `concurrency`** is enforced by an in-memory capacity
  registry with an atomic `Reserve(job_id) bool` / `Release(...)` pair,
  seeded from the manifests at startup. The reservation happens *inside* the
  claim transaction, so "this job has room" and "this worker takes that
  run" cannot be decided separately. A worker that cannot reserve a slot moves
  on to the next candidate instead of blocking.
- **Global `--workers`** is enforced twice over: there are exactly `N` worker
  goroutines, and the same capacity registry refuses reservations once `N` runs
  are executing. Default is the number of CPU cores, capped at 8.
- **After a restart**, the queue is reloaded from `run_queue` while the capacity
  counters start empty. On Linux that is correct because no child processes
  survived the restart: the parent-death signal kills the in-flight child, and
  crash recovery has already terminalised every previously `running` run, so
  nothing is executing when the first worker reserves a slot. Darwin has no
  such signal, so an orphan can still be running when its retry is claimed
  (see [Child lifetime is platform-specific](#child-lifetime-is-platform-specific)).
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
`os._exit()`-ing job cannot take the daemon down.

The child is started as `python.executable` (default `python3`) with
`main.py`'s directory as its working directory and this environment:

| Variable | Value |
| --- | --- |
| `OTTER_JOB_ID` | Durable job identity. |
| `OTTER_JOB_NAME` | Manifest label. |
| `OTTER_RUN_ID` | This attempt's run id. |
| `OTTER_API_URL` | Base URL of the daemon API (e.g. `http://127.0.0.1:7337`). |
| `OTTER_STATE_TOKEN` | Short-lived token authorizing state and log calls for this run. |
| `OTTER_TRIGGER_TYPE` | `cron`, `webhook` or `manual`. |
| `OTTER_JOB_DIR` | Absolute path of the job directory. |

plus every entry from `env` (with `${VAR}` expanded against the daemon's
environment) and every name listed in `secrets` (read from the daemon's
environment). Every inherited `OTTER_*` variable is **stripped** before the child
environment is built, so the daemon's own `OTTER_API_TOKEN` can never leak into
job code; the child sees only the scoped, per-run values above.
`PYTHONUNBUFFERED=1` and `PYTHONDONTWRITEBYTECODE=1` are set so log lines arrive
as they are written and jobs never need write access to their own
directory. `<data dir>/sdk/python` — or `--sdk-path` if set — is prepended to
`PYTHONPATH`, so `from otter import run` works with no installation step.

stdout and stderr are read line by line and written to `run_logs` as they
arrive, tagged with their stream; runtime annotations use the `otter` stream.
The exit code and `started_at`/`finished_at` are recorded on the run.

**Timeout** (`timeout`, default 300s): on expiry the executor sends `SIGTERM` to
the child's process group, waits about 5 seconds for it to exit, then sends
`SIGKILL` to the group. The run is marked `timed_out`, and the retry policy
applies. Sending the signal to the process group matters: a naive
`subprocess.run()` in a job that has spawned its own children would
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
2. Reads the finish fallback journal, if one exists. It is written only when a
   child's outcome could not be committed to SQLite, and it holds the outcome
   the attempt actually reached.
3. Finds every run still in `running` from the previous process lifetime. The
   attempt is terminalised as interrupted: it is marked `failed` with the error
   `otter daemon restarted during execution` — unless the fallback journal
   recorded an outcome for it, in which case the recorded status is applied
   instead. A successful child is therefore never re-executed because its
   terminal write failed. On Linux the child is already gone — it received
   `SIGKILL` when the daemon died — so the retry cannot overlap it. On macOS it
   may still be running; see
   [Child lifetime is platform-specific](#child-lifetime-is-platform-specific).
4. Creates the successor and its queue row in the same transaction as the
   terminal status, so a crash can never leave a terminal failure whose retry is
   missing. The successor is enqueued when the manifest's retry policy permits
   it.
5. Leaves `queued` runs queued — they simply get claimed by the new worker pool.
6. Registers cron schedules from the manifests again. Occurrences that fell in
   the downtime window are **not** replayed. If a schedule needs catch-up
   semantics, model it as state (for example a `last_processed_at` checkpoint)
   so the next run reconciles the gap.

If the fallback journal exists but cannot be read, startup recovery stops rather
than guess: a `running` run is left alone instead of being re-executed. A pending
run is visible and repairable; a duplicated external effect is not.

Recovery is fail-closed. If recovery or queue reconciliation cannot complete —
including a failure to persist the repaired state of an interrupted run — the
daemon refuses to start rather than open for new work, because continuing would
serve while accepted work sat stranded: a `running` row with no queue row, no
terminal state and no retry successor, which nothing repairs until a later
restart happens to succeed. `--allow-incomplete-recovery`
(`OTTER_ALLOW_INCOMPLETE_RECOVERY`) starts anyway and logs the error at `error`
level, for bringing a daemon up deliberately to inspect a database it cannot
read.

### Child lifetime is platform-specific

When the daemon dies abruptly — `kill -9`, an OOM kill, a panic — the fate of
the in-flight child depends on the platform. This is the one guarantee in this
document that is not uniform across supported platforms.

- **Linux.** The child is launched with `Pdeathsig: SIGKILL`
  (`internal/executor/proc_linux.go`), so the kernel kills it when the daemon's
  parent thread exits. It cannot outlive the daemon beyond signal delivery, and
  the retry startup schedules cannot overlap it. This covers the direct child
  only: a grandchild the job spawned is reparented, not signalled.
- **macOS.** Darwin has no parent-death signal. `syscall.SysProcAttr` there has
  no `Pdeathsig` field, and XNU offers no equivalent of Linux's
  `PR_SET_PDEATHSIG` (`internal/executor/proc_darwin.go`). The child is
  reparented and keeps running, while startup marks its run failed and retries
  it, so a job whose external effects are not idempotent can be
  duplicated. Closing this needs a supervisor process or a death-watch pipe
  inside the child; persisting the child's process group does not, because a
  startup sweep is post-crash cleanup rather than prevention and cannot portably
  distinguish a live child from a recycled pid.

The graceful paths are platform-independent and cover the whole process group:
timeout, cancellation and daemon shutdown send `SIGTERM`, wait out the grace
period, then send `SIGKILL` to the group. Only an abrupt daemon death leaves an
orphan, and only on macOS. The Linux half is asserted by
`internal/executor/proc_linux_test.go`; macOS is deliberately excluded because
the guarantee does not exist there. The limitation was tracked as `OT-009` and
is stated here.

Job state, run history, logs and webhook tokens all survive restarts and
reboots because they are rows in `otter.db`, not memory.

## Graceful shutdown

On `SIGTERM` (systemd stop, container stop) or `SIGINT` (Ctrl-C), `otterd`:

1. Stops the scheduler so no new cron occurrences fire.
2. Stops claiming queued runs, so the queue drains to zero workers in flight.
3. Gives every running job up to `--shutdown-grace` (default `15s`) to
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
  {"level":"info","event":"run_started","job":"shopify-to-erp","run_id":"run_01HZY3","timestamp":"2024-06-01T12:00:03Z"}
  ```

- `--log-format pretty` switches to human-readable lines for local development.
- `--log-level debug|info|warn|error` (default `info`) filters output.
- Access logs for the HTTP API are emitted as `event: "http_request"` records.

Job output is a separate concern: it is captured into the `run_logs`
table and read back with `otter logs <run-id>` / `GET /v1/runs/{id}/logs`. That
data grows without bound unless you prune it — see the retention example in
[operations.md](operations.md).

## Run timeline

A run's evidence is stored in places that share no common view: the `runs` row
(status, attempt, retry chain), `run_logs` (the daemon's own narration plus
captured stdout/stderr), and `http_exchanges` (captured HTTP summaries). Each has
its own reader, id space and timestamp column, so before the timeline the
operator was the join — reading `otter logs` to learn *when* something broke,
switching to `otter requests` to learn *what* went out, then aligning two
unrelated sequences by eye.

`otter trace <run-id>` and `GET /v1/runs/{id}/timeline` do that join. A trace is
a **read-only merge of the two event stores for one finished attempt**, assembled
in chronological order — not a new store. There is no new table, no new
ingestion and no SDK change; the only schema change is two indexes. And because
the timeline is a projection over data the daemon already keeps, it introduces no
disclosure the log and request reads do not already make (see
[the projection boundary](#the-projection-boundary)).

A trace is defined only for a **finished** attempt. A running run has no terminal
narration and its evidence is still moving, so asking for one is refused (`409`)
rather than answered with a page that is already stale. A retry is a separate run
id with a separate timeline; the chain is still read from `runs`. `include_http=false`
(CLI `--no-http`) drops the HTTP half entirely, and the cursor remembers that
setting so a continuation cannot silently change what a trace includes.

### Why the timeline needs its own indexes

Migration `0007_timeline_indexes.sql` adds `run_logs(run_id, timestamp, id)` and
`http_exchanges(run_id, occurred_at, id)`. Both tables already had `(run_id,
id)`, so the question is why a second index is needed. Because `id` is
*assignment* order, not *chronological* order: log rows arrive in bursts as the
child writes them, and capture events are ingested in batches, so neither id
sequence follows the timestamp sequence. The old indexes answer "every row for
this run" perfectly, but a page asks "the next N events after this timestamp",
and with only `(run_id, id)` the only way to answer it is to read every row for
the run and sort — which makes a trace cost as much as the run rather than as
much as the page. The new indexes turn each page into a bounded ordered seek.
They are additive: `(run_id, id)` is still what `?after_id=` streaming and the
request list use.

### Ordering across two independent sources

Events are ordered by `(at, source_rank, id)`, with `run_logs = 0` and
`http_exchanges = 1`. The rank exists because the two tables are sequenced
independently: a bare timestamp leaves two events that share a millisecond
ambiguous, and a bare id means nothing across two unrelated id spaces. A total
order is not cosmetic. A page boundary can fall inside a group of events sharing
a timestamp, and the cursor is all the next page has to go on; an ambiguous
boundary could skip or repeat one of those events.

The rank also decides what each source may return for a cursor. A source is read
with the cursor's tuple as an exclusive lexicographic bound, so its rows must
sort strictly after it. A source is queried at the cursor's own timestamp only
when that source orders at or above the cursor's rank; a lower-ranked source at
that instant has already been passed. So a cursor left on a log event still lets
a same-instant HTTP row through (the HTTP row ranks above it and sorts after),
while a cursor left on an HTTP event does not re-read same-instant log rows (they
rank below it and were already emitted). Each source is asked for one row beyond
the page, so the merge can tell a further page exists without reading either
source to its end; the limit bounds the merged page, not each source.

### The evidence revision and the continuation cursor

Pages are not a database snapshot, even for a finished attempt. Late lifecycle
lines can still land, capture ingestion can still be flushing, retention can
delete rows and startup finalization can rewrite the capture summary. Any of
those between two pages would make a continuation silently skip, duplicate or
replace an event. So the cursor carries more than a position: it carries a digest
of the evidence the first page was built from, and a continuation whose evidence
no longer matches is refused (`409`) instead of returning a trace with a hole in
it. The remedy — start the trace over — is explicit.

The digest covers the run context a page displays (status, error, exit code and
the rest of the header), the capture summary, and the extent of each source: for
logs the pair `(MIN(id), MAX(id))` plus a present/absent flag, and for HTTP the
same extent plus a digest over the retained exchange summaries. For logs the pair
is chosen over a row count deliberately. It is two covering-index seeks, whereas
`COUNT(*)` is O(rows) per page and would make the tenth page of a large run cost
about ten times the first. The pair is also sufficient: it detects every mutation
the log store can perform. An append raises `MAX(id)`, and all three deletion
paths — `DeleteForRun`, `DeleteOlderThan` and the per-job cascade —
remove whole rows, so each changes `MIN(id)` or clears both. Log rows are never
updated, and `run_logs.id` is `AUTOINCREMENT`, so a deleted id is never reused
and cannot reappear below a raised `MAX`. An in-place edit, or a write to the
database from outside the daemon, is outside what the revision claims to detect;
the log store performs neither.

The HTTP extent alone would be too weak, because an exchange row is *updated* in
place as its response lands — phase, status, duration and completeness all
change — so a metadata digest over the retained rows is folded in too. That
digest never reads a header or body column: a payload-only change does not alter
what a trace shows, and reading bodies to check evidence would turn a cheap
continuation check into a payload read.

### One connection, 250 ms

The daemon runs SQLite on a single connection (`internal/database`: WAL,
`busy_timeout(10000)`). One connection is what keeps the single-writer story
simple and the queue free of lock tuning, but it also means a reader holds the
*only* connection while it works. A timeline that took its time would stall the
log writes and capture ingestion of every executing run. A page read is therefore
bounded by an internal budget, `timeline.ReadBudget` (250 ms), covering the whole
database read: acquiring the connection, computing the revision and running the
page queries. A read that cannot finish returns `503` and no partial page,
because half a chronology is worse than a clear failure.

The 10 s busy timeout and the 250 ms budget are not the same knob. The busy
timeout is the point at which a blocked driver call gives up — a failure mode,
not the budget. The budget is the daemon's own admission bound, chosen so a
diagnostic read cannot starve writes.

The measured cost sits far inside it. On a local test fixture, a page over
100,000 log rows plus 1,000 exchanges took roughly 22–30 ms for the first page, a
continuation page and a 1,000-event page, and about 55 ms over 300,000 log rows.
The useful result is the shape rather than the figure: the revision is two index
seeks and the page is bounded by its limit, so tripling the run's rows does not
triple the read. These are local test-fixture measurements, not production
benchmarks.

### The projection boundary

The HTTP half of the timeline reads metadata columns only. It never reads a header
or body column, and the revision digest excludes bodies, so a payload-only change
cannot invalidate a cursor. Payloads stay behind `otter request <request-id>`,
which the human form names on the exchange line.

The run context is an explicit allowlisted projection (`runs.TimelineRun`) rather
than the whole run record, because `runs.Run` carries `Metadata`, which holds the
raw, unsanitized trigger body and headers as submitted. Those are not sanitized
to the standard the request read holds itself to, so they must not travel through
the timeline response or its cursor. A projection also means a field added to
`Run` later cannot silently appear in a trace.

### Where the code lives

| Piece | Location |
| --- | --- |
| Event types, cursor, revision digest, merge assembly | `internal/timeline` |
| Transaction-aware read projections | `internal/runs` (`TimelineRunTx`, `LogPageTx`, `LogEvidenceTx`) and `internal/inspection` (`TimelineExchangeTx`, `TimelineEvidenceTx`, `TimelineDigestTx`, `TimelineCaptureTx`) |
| Daemon backend method | `internal/daemon/inspection_api.go` (`TimelinePage`) |
| API route and client | `internal/api` (`GET /v1/runs/{id}/timeline`, `Client.Timeline`) |
| CLI rendering | `internal/cli/trace.go` |

The header, the revision and the page are read inside one transaction, so a page
is internally consistent even though it is not a snapshot that later pages share.

### What a trace shows

A trace event is one of three kinds:

- **lifecycle** — the runtime's own narration about the run: queued, started,
  cancelled, timed out, and the terminal summary.
- **log** — anything the job produced: captured `stdout`, `stderr`, and
  its `ctx.log` output.
- **http** — a captured exchange summary placed at its `occurred_at`: method,
  sanitized URL, status or transport-error class, duration, phase, payload
  completeness, request id and call site. Never a payload.

The first two both live in `run_logs`, and the `otter` stream carries both: the
daemon narrates there, and the SDK's structured logger posts a job's
`ctx.log` lines there too. Classifying by stream alone therefore labelled an
job's own log line a lifecycle event, which is why `run_logs` records an
`origin` (`daemon` or `child`) written at the call site
(`migrations/0008_run_logs_origin.sql`). The kind is derived from that fact, not
from the stream and not by parsing the message in normal operation. Rows written
before the column existed fall back to `runs.LooksLikeDaemonNarration`, which
recognises the fixed shapes of the narration and treats anything unrecognised as
the job's; that fallback is deliberately conservative, because
presenting a child's output as runtime narration is the more misleading error.

State mutations are **not** on the timeline in this version.
`job_state` keeps no history table, so there is nothing to place on a
chronology; adding one is a separate feature with its own capture path. Until
then a trace says nothing about state rather than implying a read or write
happened at a time it did not record.

## Why Go, why SQLite, why child processes

**Why Go.** One static binary with no runtime dependency can be scp'd to any
Linux server and started; `CGO_ENABLED=0` cross-compiles to `linux/amd64`,
`linux/arm64` and macOS from one machine. Goroutines make "N workers, each
supervising a child process" straightforward, and `os/exec` gives process-group
signalling and timeout handling without a helper library. A single process is
also the simplest thing to supervise.

**Why SQLite.** A self-hosted runtime must not require an operator to run
Postgres and Redis before their first job works. SQLite is embedded, has
no network hop, is transactional, and in WAL mode handles "one writer, many
readers" perfectly for this workload. The pure-Go driver means no cgo and no
libsqlite3 on the target host. Durability is a file-level property: back up one
file, and state, history and queue all move with it.

**Why child processes (rather than an in-process Python embed or a
container-per-run).** Child processes give the three properties that matter
most: a hard crash boundary (a job that segfaults or calls
`os._exit()` cannot take `otterd` down), a hard timeout (`SIGKILL` a process
group); and an ordinary developer workflow (run `python3 main.py` by hand,
debug it normally, no embedded interpreter to match versions with). Containers
per run would add a container runtime dependency and a lot of startup latency;
this design keeps a Raspberry Pi viable as a target.

## What is deliberately NOT here

- **No DAG engine.** Jobs are independent units. If you need
  orchestration, sequence it inside one Python program or trigger the next
  job over the API.
- **No distributed consensus / multi-node clustering.** One daemon owns one
  SQLite file. Run a second daemon with its own data directory if you need a
  second host.
- **No connectors.** There is no Salesforce/Shopify/NetSuite adapter layer, by
  design. The job *is* the connector, written in Python with whatever
  HTTP client the developer likes.
- **No UI.** The CLI and the HTTP API are the interfaces; JSON out means you can
  pipe it anywhere.
- **No plugin system, no object storage, no external secrets backend (yet).**
  The `SecretProvider` interface is designed to grow these, but today secrets
  are environment variables on the daemon.
- **No broker.** No Kafka, no Redis, no RabbitMQ. The queue is a table.
