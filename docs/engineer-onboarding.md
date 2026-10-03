# Otter Runtime Engineer Onboarding

This guide is for an engineer joining the Otter runtime repository. It explains how a command becomes a durable run, where each responsibility lives, and how to make a first change safely. The central idea is simple: Otter is a Go control plane around ordinary Python child processes. The daemon owns scheduling, durability, retries, state, and observability; Python remains an external process that communicates back over a scoped local HTTP API.

## What this repository owns

This repository contains the runtime itself:

- `otter`, the user-facing command-line client
- `otterd`, the long-running daemon
- the embedded Python SDK imported by jobs
- SQLite migrations and persistence code
- release creation, managed Python preparation, and remote deployment
- operational scripts, drills, and product contracts

It does not contain customer jobs, vendor integrations, or business mappings. Those belong in separate job projects that install and use Otter.

## The mental model

There are four objects to keep straight.

1. A **job** is a directory containing `otter.yaml` and a Python entrypoint.
2. A **release** is an immutable snapshot of that job and any declared shared Python trees.
3. A **run** is one execution attempt. A retry is a new run row linked to its predecessor.
4. The **runtime** is one Go daemon plus one SQLite database. Each attempt is launched as a fresh Python operating-system process.

The important process boundary is this:

```text
operator or scheduler
        |
        v
  HTTP API in otterd
        |
        v
 durable run row and queue row in SQLite
        |
        v
 worker claims the queue item
        |
        v
 external Python child process
        |
        +---- stdout and stderr ----> daemon ----> run_logs
        |
        +---- ctx.state and ctx.log over HTTP ----> daemon ----> SQLite
```

There is no embedded Python interpreter, container-per-run, Redis, or external message broker. Isolation comes from a separate process and process group. Durability comes from SQLite.

## How one run moves through the system

### 1 The command reaches the daemon

`cmd/otter/main.go` constructs `internal/cli.App`. Most commands resolve the current workspace, discover the daemon URL recorded under `.otter/serve`, and call the JSON API through `internal/api/client.go`. `otter validate`, `otter init`, release preparation, and daemon lifecycle commands have local work to do; ordinary runtime commands are deliberately thin clients.

`cmd/otterd/main.go` and `otter serve` both enter `internal/cli.RunDaemon`. That function combines defaults, environment variables, and flags; installs signal handling; constructs the daemon; and blocks in `Daemon.Run`.

### 2 The API normalizes the trigger

`internal/api/server.go` owns routing and authentication. Manual submissions, cron callbacks, and webhooks all end at the daemon backend as a trigger payload. The API layer understands HTTP status codes and principals; the daemon layer returns domain errors such as not found, conflict, forbidden, and paused.

The API exposes three credential scopes:

- an administrator token for control-plane operations
- a per-run token for the child process
- a per-job webhook token for webhook admission

Do not bypass these boundaries by letting the CLI or SDK read SQLite directly.

### 3 Submission binds immutable execution inputs

`internal/daemon/view.go` resolves the job's durable identity and active release. Submission reads the manifest from the active release, not from the live source tree. It records the release digest, release source directory, SDK version, identity generation, capture policy, and managed-Python environment identity on the run.

The initial run row and its queue row are inserted in one database transaction. Once that transaction commits, the request has been accepted durably. A worker notification is only a latency optimization; the persistent queue remains authoritative.

### 4 A worker claims the run

`internal/daemon/workers.go` starts a fixed goroutine pool. Each worker asks `internal/queue.Queue` for the oldest available item whose job still has concurrency capacity.

`Queue.Claim` reserves a capacity slot and deletes the queue row inside an immediate SQLite transaction. This is the handoff point: a run is never meant to be handed to two workers. If loading or marking the claimed run fails transiently, the worker re-enqueues it instead of silently stranding accepted work.

### 5 The executor launches Python

`internal/executor/executor.go` creates the child process. It selects either the manifest's external interpreter or the prepared managed interpreter, changes the working directory to the bound release, and normally launches:

```text
python -m otter._launcher main.py
```

The executor removes inherited `OTTER_*` variables so daemon credentials cannot leak into a job. It then injects the run-scoped identity, API URL, trigger type, state token, job directory, manifest environment, and resolved secrets. The embedded SDK path is first on `PYTHONPATH`.

The executor reads stdout and stderr concurrently, enforces the timeout, and terminates the entire process group with a graceful signal followed by a kill if necessary. Platform-specific process behavior is isolated in `internal/executor/proc_*.go`.

### 6 The SDK calls back into the daemon

`sdk/embed.go` embeds `sdk/python` in the Go binary and extracts it under the runtime data directory. The Python package has no third-party dependency.

`Context.from_environment` builds the job context from the scoped environment. `ctx.state` and `ctx.log` are small HTTP clients. State writes go to the daemon's `internal/state.Store`; structured logs go to the run log store. The child never opens the database, which keeps the daemon authoritative and prevents a killed Python process from corrupting runtime state.

The launcher also installs the optional outbound HTTP capture adapters. Captured exchanges are sanitized and bounded before `internal/inspection` persists them.

### 7 The outcome and any retry commit together

After the child exits, `internal/daemon/workers.go` classifies the result as succeeded, failed, timed out, or cancelled. `finishRun` decides whether the policy permits a retry.

The terminal state of the current attempt, the successor run row, and the successor queue row are committed atomically. This prevents a visible failure without its promised retry, or a retry without the predecessor's terminal result. Retry delay calculation is isolated in `internal/retry`.

If the terminal transaction cannot be persisted after bounded retries, the daemon writes a finish journal. Startup recovery applies that journal rather than guessing that the child failed.

## Codebase map

| Area | Responsibility | Start here |
| --- | --- | --- |
| `cmd/otter` and `cmd/otterd` | Minimal binary entrypoints | `main.go` in each directory |
| `internal/cli` | Command parsing, workspace discovery, output, local start and stop, release and deploy commands | `cli.go`, `daemon.go`, `start.go`, `release.go` |
| `internal/api` | HTTP routes, authentication, wire types, and the Go API client | `server.go`, `backend.go`, `client.go`, `types.go` |
| `internal/daemon` | Composition root and lifecycle coordinator | `daemon.go`, `view.go`, `workers.go`, `recovery.go` |
| `internal/config` | Daemon configuration, strict manifest parsing, validation, and discovery | `daemon.go`, `manifest.go`, `discovery.go` |
| `internal/database` and `migrations` | SQLite connection policy and schema evolution | `database.go`, `migrate.go`, numbered SQL files |
| `internal/runs` | Run records, retry chains, logs, timelines, and retention | `runs.go`, `logs.go`, `timeline.go` |
| `internal/queue` | Durable FIFO queue and atomic claim | `queue.go` |
| `internal/executor` | Child process environment, launch, output capture, timeout, and signals | `executor.go`, `proc_*.go` |
| `internal/state` | Durable per-job JSON key and value state | `state.go` |
| `internal/retry` | Deterministic backoff and attempt limits | `retry.go` |
| `internal/scheduler` and `internal/schedule` | In-process cron runner and persisted schedule overrides | `scheduler.go`, `schedule.go` |
| `internal/identity` | Durable job identity, path markers, moves, resets, deletion, and reconciliation | `service.go`, `plan.go`, `store.go` |
| `internal/release` | Content-addressed source snapshots and atomic activation | `stage.go`, `release.go`, `layout.go` |
| `internal/pyenv` | Managed interpreter and dependency environment identity and preparation | `manager.go`, `lock.go`, `egress.go` |
| `internal/secrets` | Secret-provider abstraction and environment-backed implementation | `secrets.go` |
| `internal/inspection` and `internal/timeline` | Bounded HTTP capture and merged diagnostic timelines | `store.go`, `timeline.go` |
| `internal/deploy` | Local bundle construction, remote transfer, provisioning, and systemd rendering | `deployer.go`, `render.go`, `uv.go` |
| `internal/notify` | Best-effort terminal-run notifications | `notify.go`, `format.go` |
| `sdk/python` | Dependency-free job-facing Python API and capture adapters | `context.py`, `state.py`, `log.py`, `_client.py`, `_launcher.py` |
| `scripts` and `scripts/drill` | Smoke tests, host monitoring, provisioning checks, and fault drills | `smoke.sh`, `drill.sh`, named drills |
| `docs` | User contracts, operator guidance, designs, release plans, and evidence | `architecture.md`, `runtime-contract.md`, `operations.md`, `open-work.md` |

## Durable state and ownership

The daemon uses one SQLite database in WAL mode with one open connection. The important ownership rule is that feature packages own their data operations, while `internal/daemon` owns cross-package transactions and orchestration.

The main durable records are:

- job identity and source-path ownership
- job state values
- one run row per attempt
- one queue row while an attempt is waiting
- ordered run logs
- webhook tokens and schedule overrides
- HTTP capture summaries and exchanges

Files next to the database are also part of runtime state: release snapshots, managed Python environments, the extracted SDK, the finish journal, and daemon discovery records. A backup or restore design must account for more than `otter.db`.

Schema changes use a new numbered migration. Never rewrite a migration that may already have run. A change that adds durable data must also answer how it is migrated, retained, deleted with a job, backed up, restored, and exposed safely.

## Invariants to protect

These invariants explain many implementation choices and should guide reviews.

- Accepted work is durable: a successful submission creates both the run and queue row transactionally.
- A run executes the release and environment bound at submission, including retries.
- A job label is not its identity. Durable state is keyed by identity and fenced by identity generation.
- The CLI and Python SDK use APIs; neither reads SQLite directly.
- The child receives only a per-run token, never the daemon administrator token.
- The queue claim is atomic, and a claimed run must execute, terminate durably, or be re-enqueued.
- A terminal result and its retry successor commit together.
- Reload swaps discovery state without interrupting current runs or moving unchanged cron schedules.
- Graceful shutdown stops admission and claiming before it terminates remaining children and closes SQLite.
- Invalid manifests are visible to operators but do not crash the daemon.

The normative version of these promises is `docs/runtime-contract.md`. Read it before changing run states, retries, queue behavior, recovery, identity, or release retention.

## Releases and managed Python

`otter release` snapshots code into a content-addressed directory. The snapshot reproduces the relative layout of the job and shared `python.path` trees, then activation atomically replaces a symlink. Queued work keeps the digest it was submitted with even if a newer release becomes active.

External mode runs the interpreter named by the manifest. Managed mode resolves an exact Python version, lockfile, platform, libc, and preparation policy into an environment identity. `internal/pyenv` prepares that environment with a pinned `uv`, records its metadata, and later resolves the exact environment recorded on the run.

Treat release and environment metadata as execution inputs, not caches. Removing or recomputing them casually can change what queued work executes.

## Identity and discovery

Discovery finds directories containing `otter.yaml`, but the manifest's `name` is only a human-readable label. `internal/identity` assigns a durable identifier to the source path and stores a marker in the source tree. That separation supports rename and move operations while preventing a copied or deleted-and-recreated directory from inheriting another job's state accidentally.

Identity changes bump a generation. Submission records the current generation, and both execution and per-run state mutation check it. This fences queued work and old credentials after reset, move, retirement, or deletion.

This is subtle code. Before editing it, read `docs/identity.md`, `internal/identity/plan.go`, and the identity tests as one unit.

## Observability and failure behavior

There are three distinct output paths:

- daemon operational logs, written by `internal/logging`
- child stdout and stderr, captured by the executor
- structured job logs sent by the SDK to the API

All run-associated lines are stored in `run_logs` with an origin and sequence. HTTP request capture is separate, bounded, redacted, and retention-aware. `internal/timeline` merges finished-run logs and captured exchanges for diagnostic reading without changing execution behavior.

Notifications and most diagnostic side effects occur after the durable outcome commit. They are useful but must not decide whether a run succeeded or whether a retry exists.

## Local development workflow

The supported toolchain is Go 1.22.3 or compatible with the module declaration, Python 3.13 for the SDK and process tests, Git, and Make.

Use the repository wrappers because they keep build caches inside the checkout:

```bash
make build
make test
make lint
make smoke
```

While iterating, run the narrowest package:

```bash
scripts/go test ./internal/deploy/ -run TestName -v
scripts/go test ./internal/daemon/ -run TestName -v
python3 -m unittest discover -s sdk/python/tests
```

Use `make smoke` before merging a change that could affect the CLI, releases, daemon startup, execution, or state. It builds this checkout's binaries and exercises `init` through `stop` in a temporary workspace.

Tests should create their worlds under `t.TempDir`, use loopback fake servers, and require no external credentials or vendor APIs. For concurrency-heavy changes, run the race detector on the relevant packages with `CGO_ENABLED=1`.

## Suggested reading order

1. `README.md` through the quickstart and reliability sections
2. `docs/architecture.md` for the descriptive system view
3. `docs/runtime-contract.md` for normative lifecycle guarantees
4. `cmd/otter/main.go`, `internal/cli/cli.go`, and `internal/api/server.go`
5. `internal/daemon/daemon.go`, `view.go`, and `workers.go`
6. `internal/queue/queue.go`, `internal/executor/executor.go`, and `internal/runs/runs.go`
7. `sdk/python/otter/context.py`, `state.py`, and `log.py`
8. The package related to the first assigned task

Do not try to read every implementation plan before writing code. Use them when a task touches their subject. Treat `docs/open-work.md` as an intake list, not permission to choose a product decision independently.

## Onboarding tasks

These tasks are ordered from contained to broad. Each corresponds to a currently recorded gap and avoids an unresolved product decision.

### Task 1 Make the uv cache fallback handle an existing read only cache

**Tracker:** `OT-021` in `docs/open-work.md`

**Why this is a good first task:** It is a real defect with a narrow boundary. It introduces the deploy builder, filesystem failure handling, test seams, and the project's preference for proving behavior with a regression test.

**Current behavior:** `internal/deploy/uv.go` falls back to the deploy scratch directory only when `os.MkdirAll` fails. If the user cache directory already exists but cannot accept a temporary directory, `MkdirAll` succeeds and the later `os.MkdirTemp` fails.

**Scope:**

- Add a regression test that models an existing but unwritable primary cache.
- Change cache selection so the primary location is accepted only when it can actually host the download's temporary directory.
- Preserve `OTTER_UV_CACHE` semantics deliberately: decide from the existing contract whether an explicit path should fail loudly or use the automatic fallback, and encode that expectation in the test.
- Keep the fallback inside the builder's scratch directory and clean up any write probe.
- Update `OT-021` with the test name and mark it done when the fix lands.

**Likely files:** `internal/deploy/uv.go`, a focused test under `internal/deploy`, and `docs/open-work.md`.

**Acceptance:** The new test fails on the old code, passes on the fix, `scripts/go test ./internal/deploy/...` passes, and no real network request is made.

**Expected size:** Two to four hours.

### Task 2 Make missing Python fail the daemon execution suite visibly

**Tracker:** `OT-016` in `docs/open-work.md`

**Why this is useful:** It teaches how the daemon's integration tests create real jobs and child processes, and it improves the credibility of the suite without changing production behavior.

**Current behavior:** `requirePython` in `internal/daemon/daemon_test.go` skips execution tests when `python3` is missing. Python is a documented development prerequisite and pinned in CI, so a misconfigured runner can silently omit important evidence.

**Scope:**

- Replace the silent skip with an actionable failure for tests that require Python.
- Keep the helper centralized so all daemon execution tests report the same prerequisite error.
- Add or adjust a small helper test if needed to prove the message without depending on the machine's actual `PATH`.
- Confirm the daemon package still runs with the normal toolchain.
- Update `OT-016` with the evidence and mark it done.

**Likely files:** `internal/daemon/daemon_test.go`, any focused helper test, `CONTRIBUTING.md` only if the prerequisite wording needs clarification, and `docs/open-work.md`.

**Acceptance:** A runner without `python3` gets a failing test with a clear installation or configuration message; a normal runner executes rather than skips the affected tests; `scripts/go test ./internal/daemon/...` passes locally.

**Expected size:** One to three hours.

### Task 3 Add an end to end failure notification drill

**Tracker:** `OT-023` in `docs/open-work.md`

**Why this is useful:** It walks through the whole system: daemon configuration, release and submission, child failure, durable terminal status, notification formatting, and operational evidence.

**Current behavior:** Unit tests cover notification code, but `make drill` has no committed scenario proving a real daemon sends the configured notification for a failed run and suppresses notifications outside `OTTER_NOTIFY_ON`.

**Scope:**

- Add a drill under `scripts/drill` using a local HTTP receiver as the notification endpoint.
- Start an isolated runtime and create at least one failing job and one succeeding job.
- Assert that the failed run produces exactly the expected notification and that the excluded status does not.
- Assert useful payload fields such as job identity or label, run ID, terminal status, and hostname without depending on unstable formatting.
- Make the drill self-contained, deterministic, credential-free, and loud when its platform prerequisites are absent.
- Register it with `scripts/drill.sh`, document the evidence it proves, and close `OT-023` when the drill passes.

**Likely files:** `scripts/drill/failure-notification.sh`, `scripts/drill.sh`, possibly notification test fixtures, and `docs/open-work.md`.

**Acceptance:** The drill fails when notification delivery is disabled or the status filter is broken, passes against the real built daemon, leaves no background process behind, and can run through `make drill DRILL=failure-notification`.

**Expected size:** Half a day to one day.

## Review checklist for a first change

- Can the change be explained in terms of an existing package responsibility?
- Is there a failing regression test or drill that proves the old behavior?
- Does the change preserve release binding, identity fencing, token scope, and transaction boundaries?
- Are error messages actionable without exposing secrets?
- Does the test avoid real networks, external credentials, and repository-local runtime state?
- Have the narrow tests, `make lint`, and the proportionate smoke or drill target passed?
- If user-visible behavior changed, were the product contract and open-work entry updated?

When in doubt, keep domain logic in its owning package and orchestration in `internal/daemon`. The easiest changes to review are the ones whose package boundary, durable effect, and failure behavior are all explicit.
