# Otter Runtime Contract

This document is Otter's versioned runtime contract: the promises `otterd` and
`otter` make about the work they accept, and — just as importantly — the things
they explicitly do **not** promise. It is the contract half of the `v0.2.0`
Phase 1 gate ("Dependable execution"), whose exit evidence is *a versioned
runtime contract and automated fault matrix*.

Two rules shape everything below:

- **Every guarantee names a WS4 fault-matrix scenario.** The scenario is the
  named test case that is supposed to prove the guarantee, and
  [Appendix A](#appendix-a--the-ws4-fault-matrix-and-its-evidence) records that
  scenario's evidence.
- **A guarantee with no test does not go in.** A behavior that is real but not
  yet exercised is written down as an *explicit non-guarantee*, never as a
  promise. Erring toward a smaller, true contract is deliberate.

This is a description of implemented behavior, not a design aspiration. When the
code and this document disagree, this document is wrong and is corrected to match
what the runtime actually does.

## Contents

- [Contract version and build scope](#contract-version-and-build-scope)
- [1. The attempt state machine](#1-the-attempt-state-machine)
- [2. What an accepted request promises](#2-what-an-accepted-request-promises)
- [3. Schedules, deliveries and duplicates](#3-schedules-deliveries-and-duplicates)
- [4. State-write concurrency](#4-state-write-concurrency)
- [5. Retries and external effects](#5-retries-and-external-effects)
- [6. Supported platforms](#6-supported-platforms)
- [7. Honest limits](#7-honest-limits)
- [Appendix A — the WS4 fault matrix and its evidence](#appendix-a--the-ws4-fault-matrix-and-its-evidence)
- [Appendix B — where this fits](#appendix-b--where-this-fits)

## Contract version and build scope

| Field | Value |
| --- | --- |
| Contract version | **2** |
| Introduced for | `v0.2.0` — Phase 1, "Dependable execution". |
| Amended in version 2 | `v0.4.0` — schedules are first-class and fire once per occurrence (§3.1.1), job configuration is a pinned input (§2, §7.5), and the honest limits gained §7. |
| Source revision | `ec01900` on `main`, the build version 1 described. Version 2 describes the `v0.4.0` tree. |
| Frozen at | `v1.0.0`. Before that, a **minor** release may amend this document; a patch release may not. |
| Binary pair | `otter` and `otterd` **from the same build**. The two are released together from one archive; do not mix versions. |
| Supported platforms | `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64` (static, `CGO_ENABLED=0`). Other Unix targets compile but carry no guarantee ([§6](#6-supported-platforms)). |
| Python | Python 3 on `PATH` for `python.mode: external` (the default). `managed` mode prepares its own interpreter. |
| Durable store | One SQLite file, `<data dir>/otter.db`, WAL mode. The child never opens it; the daemon is the single writer. |

How this version is verified:

```bash
go test ./...                                        # the fault matrix and unit suites
python3 -m unittest discover -s sdk/python/tests     # the embedded SDK
make smoke                                           # otter init → … → stop, end to end
```

Versioning rules:

1. Contract version increments on **any** change to a promise or a non-promise
   in this document, including a narrowing.
2. Contract version is independent of the product semver. Product `v0.2.x`
   patches may fix defects but may not change what this document promises.
3. API shape, manifest fields, CLI JSON and error codes are pinned separately,
   under the `v0.4.0` compatibility policy: [compatibility.md](compatibility.md)
   states what a breaking change is for each interface and how a deprecation
   retires. This document pins *behavior*; that one pins *shape*. The
   machine-readable form is `GET /v1/version` (or `otter --json version`),
   which reports `contract_version`, `schema_version`, the manifest schema, the
   SDK version and the supported platforms together.
4. A guarantee is only as strong as its scenario's evidence. Appendix A states
   whether each scenario is real, simulated, partial or not yet implemented.

> **Reading the tables.** *Guarantee* rows are promises backed by at least one
> named scenario. *Explicit non-guarantee* rows are behaviors the runtime does
> not promise; they need no test, and the absence of one is not a defect.

## 1. The attempt state machine

One row in `runs` is one **attempt**. A retry is a new row with a new id and a
`parent_run_id` pointing at the attempt it follows; it never mutates the old
row. Every attempt carries `status`, `attempt`, `parent_run_id`, timestamps and
an outcome.

| Status | Terminal | Meaning |
| --- | --- | --- |
| `queued` | no | Accepted and durable; a `run_queue` row exists; not yet claimed. |
| `running` | no | Claimed and `started_at` recorded; a child process was launched. |
| `retrying` | no | A future attempt created by a failed predecessor; it is parked in the queue with a future `available_at` and is not yet claimable. |
| `succeeded` | yes | Child exited 0. |
| `failed` | yes | Non-zero exit, a configuration/start failure, a graceful-shutdown kill, or an interrupted run terminalised by recovery. |
| `cancelled` | yes | Operator cancellation, an identity change, or a quarantined release. Never retried. |
| `timed_out` | yes | The attempt exceeded its `timeout`. Retried only if the policy allows. |

`queued`, `running` and `retrying` are the non-terminal set; the other four are
terminal and are the only values `runs.Finish` accepts.

### Legal transitions

The daemon only ever produces these transitions. Anything else is a defect.

| From | To | Trigger | Where |
| --- | --- | --- | --- |
| — | `queued` | Submission (manual, webhook, cron), recorded with its queue row in one transaction. | `daemon/view.go` `SubmitRunWithOptions` |
| — | `retrying` | Successor of a failed attempt the policy allows, written with the terminal state. | `daemon/workers.go` `planRetry` / `persistOutcome` |
| `queued` | `running` | Atomic claim, then `MarkRunning`. | `queue.Claim`, `runs.MarkRunning`, `daemon/workers.go` |
| `queued` | `cancelled` | Cancelled before execution, an identity that no longer accepts work, or a release quarantine. | `daemon/view.go` `CancelRun`, `daemon/workers.go`, `runs.CancelPinnedToRelease` |
| `queued` | `failed` | The job or its bound release disappeared, or its environment could not be resolved, before the child started. | `daemon/workers.go` `executeRun` |
| `retrying` | `running` | Claim after `available_at`, then `MarkRunning`. | `queue.Claim`, `runs.MarkRunning` |
| `retrying` | `cancelled` | Cancelled during backoff, or quarantined. | `daemon/view.go` `CancelRun`, `runs.CancelPinnedToRelease` |
| `running` | `succeeded` | Child exited 0. | `daemon/workers.go` `classifyOutcome` |
| `running` | `failed` | Non-zero exit, start error, graceful-shutdown kill, restart recovery, or a finish write recovered from the fallback journal. | `daemon/workers.go`, `daemon/recovery.go`, `daemon/finishjournal.go` |
| `running` | `cancelled` | Operator cancellation or an identity change observed after claim. | `daemon/workers.go` |
| `running` | `timed_out` | The executor timeout fired. | `daemon/workers.go` |

Guarantees:

- **`MarkRunning` is the only gate into `running`, and only from `queued` or
  `retrying`.** A terminal attempt and an already-`running` attempt are both
  refused, so an attempt cannot be executed twice through this path.
  *Scenario FM-09.*
- **A terminal attempt is never re-claimed.** Recovery scans only `running`
  rows, and the queue no longer holds a finished run. *Scenario FM-09 / FM-01.*
- **An attempt's terminal state and its successor are one durable decision.**
  A retry is never left missing, and a terminal failure never appears without
  the successor it implies. *Scenario FM-02.*

> **Storage-layer caveat.** `runs.Finish`, `runs.FinishTx` and `runs.SetStatus`
> are unguarded primitives: `FinishTx` requires a *terminal target* but has no
> current-status predicate, and `SetStatus` accepts any valid status. The
> immutability above is enforced by the daemon's call paths, not by the store.
> A future administrative caller must preserve the machine; the store will not
> stop it. This is stated so the guarantee is not mistaken for a database
> constraint.

## 2. What an accepted request promises

A request is **accepted** when the API answers `202 Accepted` with a run id and
`"status": "queued"`, or — for a cron occurrence — when the daemon's internal
submission returns successfully. The submitting endpoints are:

| Trigger | Entry point |
| --- | --- |
| Manual / CLI | `POST /v1/jobs/{id}/runs` |
| Webhook | `POST /v1/hooks/{job}` |
| Cron | the in-process scheduler, one run per occurrence |

Guarantees of acceptance:

- **Acceptance is durable or it is an error.** The attempt row and its
  `run_queue` row are written in one transaction (`daemon/view.go`). A crash
  immediately after the response cannot lose the run, and a failed transaction
  returns an error with no half-created attempt. *Scenario FM-01.*
- **Queued work survives restart and reboot**, because it is rows in
  `otter.db`, not memory. *Scenario FM-01.*
- **Waiting work with no queue row is repaired at startup.** Reconciliation
  re-enqueues every `queued`/`retrying` attempt that lost its queue row,
  exactly once. *Scenario FM-01.*
- **Refusal is explicit and inspectable.** A refused request returns a typed
  error — unknown or invalid job, no active release, paused for
  cron/webhook, or a draining daemon — rather than a silent no-op. *Scenario
  FM-01.*
- **Every accepted attempt reaches a terminal status.** It either finishes, or
  is terminalised at startup if it was interrupted, and a retryable failure
  produces a successor. *Scenario FM-01 / FM-02.*
- **Every input a run depends on is pinned at submission.** The release digest,
  the prepared-environment digest, and — since `v0.4.0` — the job
  **configuration version** are recorded on the attempt, and execution resolves
  them from the attempt, never from "whatever is current then". Changing a
  job's configuration writes a new immutable version and leaves accepted work
  alone; a retry inherits its parent's version.
  `TestSubmitPinsJobConfigAndRetryKeepsIt`.
  Configuration values are deployment inputs, not secrets: see
  [§7.5](#75-configuration-is-not-a-secret-store).

What acceptance does **not** promise:

- when execution starts (the queue is FIFO among *eligible* work, and a
  saturated job or worker pool defers it);
- that the attempt succeeds;
- that the attempt runs only once (see [§5](#5-retries-and-external-effects));
- that two submissions are collapsed into one ([§3](#3-schedules-deliveries-and-duplicates)).

## 3. Schedules, deliveries and duplicates

### 3.1 Missed cron windows are not replayed

*Explicit non-guarantee.*

The scheduler fires while the daemon is running, and each occurrence enqueues
one run carrying its `scheduled_at` in the run metadata. Occurrences that fall
during downtime are **not** replayed at startup. The scheduler's own package
documents this as deliberate; there is no catch-up pass.

A job that needs catch-up semantics must model it as durable state —
for example a `last_processed_at` checkpoint written from `ctx.state` — and
reconcile the gap on its next run.

Two related consequences:

- **Downtime skips; slowness accumulates.** Occurrences during uptime always
  enqueue, even if a previous run is still going, so a long run on a short
  schedule builds a backlog rather than skipping work. With the default
  `concurrency: 1` that backlog serializes. This is tracked for a future
  decision as `OT-007` / `OT-008` in [open-work.md](open-work.md).
- **A backlog runs its bound release.** Each attempt records the release digest
  at submission, so a deep backlog executes the code that was active when each
  occurrence was accepted, not the newest release. Submission-time binding is
  tested by `TestRunExecutesTheActiveReleaseNotTheLiveTree`; the retry half is
  not yet independently proven (`OT-011`).
- **Releases still needed by pending work are protected from retention, and a
  submission cannot slip past the pin set.** The CLI retention pass collects the
  digests bound to every `queued`, `running` and `retrying` attempt
  (`internal/cli/release.go` `pinnedReleases`) and plans their removal with
  `release.PlanRetain`, which renames a doomed release out of the live tree. The
  pin query and the rename run inside **one immediate transaction** — the
  database write lock — and the deletion (`Cleanup`) happens only after it
  commits. A submission binds its release in its **own** immediate transaction
  and re-checks that the release directory still exists there
  (`internal/daemon/view.go`, `submitRun`). The two order rather than
  interleave: a run committed before the prune is in the pin set and protected,
  and a submission that commits after it is refused with a conflict naming the
  vanished release instead of queueing work against a snapshot that no longer
  exists. This closes `OT-010`.

### 3.1.1 One run per occurrence, and which clock decides

*Guarantee.*

Each schedule keeps an occurrence ledger (`schedule_fires`, keyed by
`(schedule_id, occurrence_at)`). The ledger row is written in the **same
transaction** that creates the run and its queue entry, so:

- a crash after the run is accepted cannot double-fire — the ledger row
  committed with it;
- a duplicate wake-up (a reload racing a fire, or the same occurrence delivered
  twice) loses to the ledger's primary key and produces **no second run**;
- a crash before the run is accepted leaves neither a run nor a ledger row, so
  the next occurrence is simply in the future.

Retries are unaffected: a retry is a new `runs` row referencing
`parent_run_id`, not a new occurrence, so it never touches the ledger.

**A schedule's cron is interpreted in its own IANA `timezone`, and new schedules
default to UTC.** This is a deliberate change from v0.3.0, where a manifest's
`trigger.cron` was interpreted in the **host's local time**. Host-local time is
not a contract anyone can hold: the same manifest meant a different instant
after a host was rebuilt with a different clock, and a control plane writing
`0 3 * * *` had no way to say whose 03:00 it meant. A manifest that wants its
old local-time meaning must now say so explicitly, e.g.
`trigger.cron: "0 3 * * *"` with the schedule's time zone set to the zone the
host used to be in, or the equivalent `CRON_TZ=<zone>` form in the expression.
DST follows the named zone: a `0 2 * * *` schedule may skip or repeat an hour
twice a year, which is the cron norm rather than a special case.

The ledger prunes with the runs window: an occurrence older than the run
retention cutoff has already happened and can never fire again, so dropping it
cannot make a restart replay anything.

### 3.2 Duplicate webhook delivery is not deduplicated

*Explicit non-guarantee.*

Every authenticated `POST /v1/hooks/{job}` creates a new attempt. Otter
has no delivery id, no idempotency key and no dedup table, so a redelivery — a
retrying sender, an at-least-once queue, a human clicking twice — produces a
second run and therefore a second execution of the job's effects.

The `202` response carries the new run id so the caller can correlate its
delivery with what ran. Deduplication is the caller's responsibility; make the
job's external effects idempotent
([§5.3](#53-idempotency-guidance)) or set `concurrency: 1` and checkpoint the
last processed delivery id in state.

### 3.3 Delivery semantics in one line

Otter's execution semantics are **at-least-once in effect**: accepted work is
durable, and a retry re-runs the attempt and therefore its external effects.
Exactly-once is not promised anywhere in this document.

## 4. State-write concurrency

`ctx.state` is a durable per-job key/value store in SQLite, reached by
the child over HTTP with a short-lived per-run token. Keys are namespaced by the
durable job identity, so a recreated job does not inherit a
previous one's values.

Guarantees:

- **`set` is a single-statement upsert.** Writing the same key twice leaves one
  row whose value is the later write and whose `updated_at` advances. *Scenario
  FM-07.*
- **Concurrent writes to different keys do not lose or cross-contaminate
  rows.** Twenty concurrent writers to twenty keys all persist with their own
  values. *Scenario FM-07.*
- **A write from a stale identity generation is refused** with `409 Conflict`,
  while a token at the current generation is unaffected. This fences a run that
  survived a reset, move, retirement or deletion. *Scenario FM-07.*

### 4.1 There is no compare-and-swap

*Explicit non-guarantee.*

`ctx.state.set` is a plain upsert (`internal/state/state.go`). It carries no
version, ETag or conditional predicate. A read-modify-write — `get`, compute,
`set` — is therefore **not atomic**: two attempts in the same generation can
both read the same value and the later write silently wins. Identity-generation
fencing is not a value CAS; it only rejects a token minted for a different
identity generation.

Because of this, and because the same hazard applies to any checkpoint an
job keeps in state:

- **`concurrency: 1` is the safe default** for any job whose state is
  read-modify-write, including counters, cursors and checkpoints. It is also the
  manifest default.
- Safe patterns at `concurrency > 1` are limited to writes that cannot conflict:
  a disjoint key per attempt, or append-only/unique-key records. A shared
  counter or a single checkpoint key must stay at `concurrency: 1`, or be
  protected by a lock the job owns outside `ctx.state`.

The operational form of this guidance is in
[operations.md](operations.md#capacity-and-concurrency-tuning); the manifest
field is documented in
[manifest-reference.md](manifest-reference.md#timeouts-and-concurrency).

## 5. Retries and external effects

### 5.1 What is retried, and what is not

An attempt is succeeded or failed by `classifyOutcome`, and a failure is retried
by the manifest's policy (`attempts`, `backoff`, `initial_delay`, `max_delay`).
The retry decision is made from the policy alone, before any side effect, and
the successor is committed with the terminal state.

**Retried** (subject to the policy): a non-zero child exit, a timeout, a run
killed by graceful shutdown, and a run terminalised as interrupted by crash
recovery.

**Never retried** (retrying would repeat the same mistake): an operator
cancellation, a missing or non-executable entrypoint, an invalid manifest, a
secret listed in `secrets` that is missing from the daemon environment, and a
run cancelled by an identity change or release quarantine.

Guarantees:

- **Backoff is deterministic and lives in the queue row.** There is no jitter:
  `exponential` with `initial_delay: 2s` means 2s, 4s, 8s, saturating at
  `max_delay`. The delay is stored as the successor's `available_at` rather than
  an in-memory timer, so it is part of the durable queue and survives a restart
  (the persisted-deadline half is not yet crash-tested; see FM-04 in
  [Appendix A](#appendix-a--the-ws4-fault-matrix-and-its-evidence)). *Scenario
  FM-04.*

**Retry release and environment binding.** Every attempt records the release
digest and source directory, the Python mode and version, and — for managed
Python — the prepared interpreter and environment digest (`daemon/view.go`). The
first attempt resolves those at submission; a retry successor copies them from
its parent rather than re-resolving them from the live tree
(`daemon/workers.go` `planRetry`), and execution re-reads the manifest from the
attempt's *bound* snapshot and rebuilds the recorded environment identity
(`daemon/workers.go` `executeRun`). The retry policy — whether a successor is
owed, and how long it waits — is decided from the bound release's manifest too,
both on the ordinary path and when crash recovery plans a successor for an
interrupted run (`daemon/recovery.go` `recoveryRetryPolicy`). Activating a newer
release, changing the interpreter, or changing the retry policy while a retry is
pending therefore cannot move that retry onto different code, a different
environment, or a different policy.

If the bound snapshot is unreadable at execution time the attempt fails with that
cause rather than falling back to the live tree or the active release, and crash
recovery grants such a run no successor. That last behaviour is deliberate and
carries a stated cost: recovery cannot tell a permanently missing snapshot from a
transiently unreadable one, so a snapshot that is unreadable during recovery and
readable moments later loses a retry it would otherwise have run. The warning
names the run; restoring the snapshot later does not bring the attempt back. The
only case that consults the live manifest for execution or retry policy is a run
with no release binding at all — a legacy run, or one submitted before releases
existed. The capture policy is a separate decision, deliberately read from the
live manifest (§5.3).

Pinned by `TestRunExecutesTheActiveReleaseNotTheLiveTree` (submission binding),
`TestRetryExecutesTheParentsReleaseSnapshot` and
`TestRetryResolvesTheParentsManagedEnvironment` (the retry half, including under
managed Python), `TestPendingBacklogKeepsItsDigestThroughAReleasePrune` (through
a retention pass — it pins the `Retain` contract with the same query the CLI
uses, and the CLI entry point is covered by
`TestReleaseKeepPrunesToTheWindowAndProtectsPins`), and
`TestRecoveryPlansRetriesFromTheBoundRelease` (both directions: a looser live
policy must not extend a bound run's budget, and a stricter one must not withhold
the retry the bound release promises). This closes `OT-011`.

### 5.2 Exactly-once execution is not promised

*Explicit non-guarantee.*

A retry re-runs the same entrypoint. Any external effect the earlier attempt
already performed — an API call, a row inserted, an email sent, a file written
— may therefore happen again. This applies to every retry path: a non-zero exit,
a timeout, a graceful-shutdown kill, restart recovery, and (on macOS) an abrupt
daemon death that leaves the first child running while its retry starts. Otter
does not inspect or undo external state to decide whether an effect already
landed.

Otter's honest description is **at-least-once execution with a durable audit
trail**. Each attempt is a row, and `GET /v1/runs/{id}` returns the full retry
chain, so a caller can see exactly which attempts ran.

### 5.3 Idempotency guidance

Because exactly-once is not promised, jobs should be written so a repeat
is harmless:

- **Upsert by a stable external id** rather than inserting. This is the same
  discipline the runtime uses for its own writes.
- **Send an idempotency key** derived from stable input (for example the run's
  business key, not the run id) where the downstream API supports one.
- **Record progress in `ctx.state` after the external effect**, and reconcile
  from that checkpoint on the next attempt. Keep such a checkpoint at
  `concurrency: 1`.
- **Prefer small, replayable units of work** so a retry re-does one page, not a
  whole sync.
- **Treat a duplicate effect as expected, not exceptional**, and make the
  downstream observable state converge.

## 6. Supported platforms

Otter ships two static executables — `otter` and `otterd` — for four platform
targets:

| Platform | Child process group | Timeout / cancel / graceful shutdown kill the group | Abrupt daemon death kills the direct child | Underlying mechanism |
| --- | --- | --- | --- | --- |
| `linux/amd64`, `linux/arm64` | yes | yes | **yes** | `Setpgid` + `Pdeathsig: SIGKILL` |
| `darwin/amd64`, `darwin/arm64` | yes | yes | **no** | `Setpgid` only |
| other Unix (compiles only) | yes | yes | no | `Setpgid` only |

Guarantees that hold on **every** supported platform:

- Each run is its own process group, so timeout, cancellation and graceful
  shutdown signal the whole group (SIGTERM, a ~5s grace, then SIGKILL). An
  job that spawns its own children does not leave them behind on these
  paths. *Scenario FM-03 / FM-05.*
- Cancellation marks the attempt `cancelled` and does not retry it. *Scenario
  FM-05.*

The one platform-specific guarantee:

- **Linux: the direct child cannot outlive an abruptly killed daemon.** The
  child is launched with `Pdeathsig: SIGKILL`, so `kill -9`, an OOM kill or a
  panic on the daemon kills it before its retry can overlap it. This covers the
  direct child only; a grandchild the job spawned is reparented and is
  covered only by the graceful group paths. *Scenario FM-03.*

### 6.1 macOS abrupt-death exposure

*Explicit non-guarantee.*

Darwin has no parent-death signal: `syscall.SysProcAttr` has no `Pdeathsig`
field and XNU has no equivalent of `PR_SET_PDEATHSIG`. After an abrupt daemon
death on macOS the in-flight child is reparented and keeps running, while
startup marks its attempt failed and retries it. The same work can therefore run
twice, concurrently, and a job whose external effects are not
idempotent can be duplicated. Closing this needs a supervisor process or a
death-watch pipe inside the child; persisting a process group is not enough,
because a startup sweep is post-crash cleanup and cannot portably distinguish a
live child from a recycled pid.

This is documented as a limitation, not fixed, and is tracked as `OT-009`
(closed) in [open-work.md](open-work.md) and in
[architecture.md](architecture.md#child-lifetime-is-platform-specific).

## 7. Honest limits

Five limits a consumer of this contract must know. Each names where the
mechanism lives; none is a promise, and none is fixable by a test alone.

### 7.1 Concurrent updates are not coordinated

Releasing is atomic: a release is staged, then activated by swapping a symlink,
and a run binds the digest it will execute at submission. Activating a new
release therefore never moves an accepted run onto different code
([§2](#2-what-an-accepted-request-promises)).

What is **not** coordinated is two update passes running at once. `otter deploy`
and `otter release` are converges and are safe to re-run, but two invocations
against the same workspace may interleave their staging and retention steps and
report each other's work. SQLite serialises their database work and
[retention is serialised against submission](#3-schedules-deliveries-and-duplicates)
(`OT-010`), but the filesystem steps are not mutually excluded. **One writer at a
time is the supported operation**; there is no distributed lock or leader
election.

### 7.2 A retry can duplicate an external effect

*Explicit non-guarantee, stated in full in [§5](#5-retries-and-external-effects).*

Otter guarantees at-least-once execution of an accepted attempt; it does not
deduplicate the *effects* of that execution at the receiving system. A job whose
side effect is not idempotent can apply it twice after a retry or a crash
recovery. The job must carry its own idempotency key or checkpoint; §5.3 is the
guidance. This is the same exposure every at-least-once runner has, written down
rather than implied.

### 7.3 A downgrade is refused, not supported

Migrations are append-only. A database migrated by a **newer** binary is refused
at startup (`SchemaTooNewError`) and the operator restores the pre-upgrade
backup; columns may have been renamed or dropped, and a query that no longer
means what it says is how a downgrade corrupts data
([database/migrate.go](../internal/database/migrate.go)). There is no
down-migration, and none is planned. Install the newer binary and restore the
backup, or stay put.

### 7.4 The transport is plain HTTP

`otterd` is an HTTP server with no TLS listener, by design. TLS is terminated in
front of it — a reverse proxy on the same host, or a private network that never
leaves the operator's control. A non-loopback bind requires a bearer token, and
since `v0.4.0` it also requires `otter deploy`'s explicit `--allow-remote-bind`
and a warning; but the token travels in clear text, and there is no mutual TLS,
no client certificate and no payload encryption between the client and the
daemon. Do not put the API on an untrusted network without a proxy that
terminates TLS: see
[security.md](security.md#remote-access-tls-and-reverse-proxies) for the
supported pattern.

### 7.5 Configuration is not a secret store

*Explicit non-guarantee.*

Job configuration (`job_configs`, `ctx.config`) is **not** encrypted, not
redacted, and not treated as sensitive: it is returned by
`GET /v1/jobs/{id}/config` to any scoped reader and printed in
`ctx.config`. A secret belongs in the deployment's secret store
(`otter.env`, `otter.daemon.env`), never in configuration. Two smaller limits
follow from the same design:

- **No environment projection in `v0.4.0`.** A configuration key reaches the job
  only through `ctx.config`; the daemon does not expand it into a named
  environment variable, so a configuration entry cannot shadow a manifest `env`
  entry. A manifest opt-in for projection may be added in a later minor.
- **Versions are retained, not garbage-collected.** Writing configuration mints
  an immutable version and does not delete superseded ones, because a queued,
  retrying or backlogged run may still be pinned to one. They are removed only
  when the job's identity is purged (`otter delete`). A job whose configuration
  changes very frequently should expect the table to grow with it.

## Appendix A — the WS4 fault matrix and its evidence

WS4 names the scenarios the runtime's failure behavior is judged against, in
[the `v0.2.0` release plan](archive/v0.2.0-release-plan.md#new-capability-the-fault-matrix).
The plan fixes eight as the minimum; `FM-01`–`FM-08` are those, and `FM-09` is
added here for the attempt state machine itself, which the other scenarios
exercise but do not state as a scenario. The IDs are assigned by this contract so
guarantees can cite them; the names are the release plan's, except `FM-09`,
which is this document's. Evidence status is deliberately literal:

- **Real** — spawns real processes and kills one.
- **Simulated** — asserts the behavior by seeding state or injecting a fault
  in-process.
- **Partial** — proves part of the scenario only.
- **None** — no automated scenario yet.

| ID | Scenario | Evidence | Anchor tests |
| --- | --- | --- | --- |
| FM-01 | SIGKILL the daemon mid-run; every interrupted run reaches a terminal state or is re-enqueued; >50 at once | **Real (Linux)** | `TestCrashHarnessSIGKILLAndRecovery` boots the real daemon as a separate OS process against a temporary data directory, submits 80 runs, waits until more than 50 children are genuinely executing rather than queued, `SIGKILL`s it, restarts over the same data directory, and asserts every accepted run terminal across 144 rows with all 64 interrupted runs re-enqueued. On Linux it additionally asserts every recorded interrupted child died with the daemon (`Pdeathsig`) and every accepted chain completed exactly once — removing `Pdeathsig` makes it fail — so the row is Real on the platform CI runs, and the no-duplicate half stays the Darwin non-guarantee ([architecture.md](architecture.md#child-lifetime-is-platform-specific)). Seeded anchors kept: `TestCrashRecoveryMarksRunningRunsFailedAndRetries`, `TestCrashRecoveryHandlesMoreThanOneListingPage` (240 seeded rows), `TestCrashRecoveryWithoutRetryPolicyLeavesRunFailed`, `TestStartupFailClosedOnIncompleteRecovery`, `TestReconcileQueueReenqueuesMoreThanOneListingPage`. Admission/refusal: `TestSubmitRunRejectsUnknownAndInvalidJobs`, `TestUnreleasedJobIsRefused`, `TestDrainingRuntimeRejectsNewRuns`, `TestPausedWebhookIsUnavailableButManualRunsStillWork`. A submit→close→reopen→read durability test: still none. |
| FM-02 | SIGKILL the daemon between finish and retry; no terminal failure without its retry | **Real (atomicity; retry binding still `OT-011`)** | `TestCrashHarnessSIGKILLBetweenFinishAndRetry` holds the finish transaction open through the existing `finishCommit` seam after the child has exited, `SIGKILL`s the daemon inside that window, restarts, and asserts the interrupted attempt is terminal **with** its successor — deterministically, and red if recovery drops the successor. It deliberately does **not** claim the attempt ran only once: the child is re-executed, which §2 and §5.2 already disclaim. Seeded anchors kept: `TestFinishRunPersistsOutcomeAndSuccessorAtomically`, `TestFinishCommitFailureLeavesNoPartialOutcome`, `TestFallbackJournalIsAppliedOnRestart`, `TestUnreadableFallbackJournalDoesNotReRunRunningRuns`. Retry release/environment binding: still open (`OT-011`). |
| FM-03 | Kill the child, leave the daemon; attempt recorded, descendants do not survive | **Partial** | Linux direct child: `TestChildDiesWhenDaemonIsKilled` (real SIGKILL of a stand-in, no DB). In-process child death: `TestTimeoutMarksRunTimedOut`, `TestTimeoutIsRetriedWhenPolicyAllows`. Attempt-recorded-under-a-real-kill: none. |
| FM-04 | Crash during retry backoff; retry claimable after `available_at`, backoff preserved | **Partial** | `TestClaimRespectsAvailableAt`, `TestRetryPolicyRetriesUntilSuccess`, `TestClaimedRunIsRequeuedWhenMarkRunningFails`. A real restart mid-backoff: none. |
| FM-05 | Cancel a running run; process group dies, status `cancelled`, not retried | **Partial** | `TestCancelRunningRunIsNotRetried`, `TestCancelQueuedRunRemovesItFromTheQueue`. Process-group death under cancel: none. |
| FM-06 | Two processes claim concurrently; no run executed twice | **Simulated** | `TestClaimConcurrentNoDoubleExecution` (8 goroutines, one `MaxOpenConns(1)` connection). A second process or connection: none. |
| FM-07 | `concurrency: N` state updates match documented behavior | **Partial** | Parallelism observed: `TestConcurrencyLimitSerializesRuns`, `TestConcurrencyAboveOneRunsInParallel`, `TestWorkerPoolLimitCapsGlobalConcurrency`, `TestCapacityReserveRelease`, `TestClaimCapacityIsPerJob`. State: `TestConcurrentWriters` (distinct keys), `TestStateEndpoints`, `TestStateWriteRefusesAStaleGeneration`, `TestStateAPIRoundTripAndNamespacing`. A same-key read-modify-write race: none. |
| FM-08 | Disk exhaustion / oversized output; failures visible, bounded, recoverable | **Partial** | Bounded output/input: `TestRunTruncatesVeryLongLines` (`internal/executor`), `TestRequestBodyTooLarge`. Disk exhaustion (ENOSPC) and recovery: none. |
| FM-09 | The attempt state machine admits only legal transitions; a terminal attempt is never resurrected | **Partial** | Classification: `TestStatusSemantics`, `TestCreateRejectsAnInvalidStatus`, `TestSetStatus`. Guards: `TestMarkRunningOnlyTransitionsClaimableRuns` (terminal and `running` not claimable), `TestFinishRecordsOutcome`, `TestFinishTxJoinsTheCallersTransaction` (non-terminal finish rejected). A terminal row being overwritten: none — the store primitive is unguarded (see the caveat in §1). |

### What Appendix A means for the guarantees

Every guarantee in §§1–6 cites at least one scenario that has at least a
Partial test, and the untested half of a Partial scenario is written as an
explicit non-guarantee rather than a promise. No scenario below is yet at the
strength the release plan intends:

- **FM-06** still needs a second connection or process for the claim race.
  FM-01 and FM-02 now run against a real `SIGKILL` of a real daemon process
  (`TestCrashHarnessSIGKILLAndRecovery`, `TestCrashHarnessSIGKILLBetweenFinishAndRetry`).
  What is *not* covered there is stated rather than implied: on Darwin the
  no-duplicate half is the [child-lifetime
  non-guarantee](architecture.md#child-lifetime-is-platform-specific), and FM-02
  does not yet prove that a retry re-uses its parent's bound release and
  environment (`OT-011`).
- **FM-03, FM-04, FM-05, FM-08** need the real crash/kill halves of the
  scenario.
- **FM-07** needs a same-key read-modify-write race to turn the
  [§4.1](#41-there-is-no-compare-and-swap) warning into a measured result.
- **FM-09** needs a guard in the store, or a test, that a terminal row cannot be
  overwritten.

As the remaining rows land, this appendix moves from Partial/Simulated to Real
and the contract version increments.

## Appendix B — where this fits

| Question | Document |
| --- | --- |
| How the runtime is built and why | [architecture.md](architecture.md) |
| Every `otter.yaml` field, including `concurrency` and `retry` | [manifest-reference.md](manifest-reference.md) |
| Every endpoint, credential and error code | [api-reference.md](api-reference.md) |
| Operating guidance, capacity tuning and troubleshooting | [operations.md](operations.md) |
| The work this contract is the evidence for | [v0.2.0-release-plan.md](archive/v0.2.0-release-plan.md) |
| Phase gates and the freeze release | [product-roadmap.md](product-roadmap.md) |
| Known open defects and unscheduled work | [open-work.md](open-work.md) |
