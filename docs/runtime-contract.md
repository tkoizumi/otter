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
- [Appendix A — the WS4 fault matrix and its evidence](#appendix-a--the-ws4-fault-matrix-and-its-evidence)
- [Appendix B — where this fits](#appendix-b--where-this-fits)

## Contract version and build scope

| Field | Value |
| --- | --- |
| Contract version | **1** |
| Introduced for | `v0.2.0` — Phase 1, "Dependable execution". |
| Source revision | `ec01900` on `main`, the build this version describes. |
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
   under the `v0.4.0` compatibility policy. This document pins *behavior*.
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
| `queued` | `failed` | The integration or its bound release disappeared, or its environment could not be resolved, before the child started. | `daemon/workers.go` `executeRun` |
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
| Manual / CLI | `POST /v1/integrations/{id}/runs` |
| Webhook | `POST /v1/hooks/{integration}` |
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
  error — unknown or invalid integration, no active release, paused for
  cron/webhook, or a draining daemon — rather than a silent no-op. *Scenario
  FM-01.*
- **Every accepted attempt reaches a terminal status.** It either finishes, or
  is terminalised at startup if it was interrupted, and a retryable failure
  produces a successor. *Scenario FM-01 / FM-02.*

What acceptance does **not** promise:

- when execution starts (the queue is FIFO among *eligible* work, and a
  saturated integration or worker pool defers it);
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

An integration that needs catch-up semantics must model it as durable state —
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
  occurrence was accepted, not the newest release.

### 3.2 Duplicate webhook delivery is not deduplicated

*Explicit non-guarantee.*

Every authenticated `POST /v1/hooks/{integration}` creates a new attempt. Otter
has no delivery id, no idempotency key and no dedup table, so a redelivery — a
retrying sender, an at-least-once queue, a human clicking twice — produces a
second run and therefore a second execution of the integration's effects.

The `202` response carries the new run id so the caller can correlate its
delivery with what ran. Deduplication is the caller's responsibility; make the
integration's external effects idempotent
([§5.3](#53-idempotency-guidance)) or set `concurrency: 1` and checkpoint the
last processed delivery id in state.

### 3.3 Delivery semantics in one line

Otter's execution semantics are **at-least-once in effect**: accepted work is
durable, and a retry re-runs the attempt and therefore its external effects.
Exactly-once is not promised anywhere in this document.

## 4. State-write concurrency

`ctx.state` is a durable per-integration key/value store in SQLite, reached by
the child over HTTP with a short-lived per-run token. Keys are namespaced by the
durable integration identity, so a recreated integration does not inherit a
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
integration keeps in state:

- **`concurrency: 1` is the safe default** for any integration whose state is
  read-modify-write, including counters, cursors and checkpoints. It is also the
  manifest default.
- Safe patterns at `concurrency > 1` are limited to writes that cannot conflict:
  a disjoint key per attempt, or append-only/unique-key records. A shared
  counter or a single checkpoint key must stay at `concurrency: 1`, or be
  protected by a lock the integration owns outside `ctx.state`.

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
- **A retry runs its parent's bound release and environment**, not the live
  tree and not whatever is active later. *Scenario FM-02.*

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

Because exactly-once is not promised, integrations should be written so a repeat
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
  integration that spawns its own children does not leave them behind on these
  paths. *Scenario FM-03 / FM-05.*
- Cancellation marks the attempt `cancelled` and does not retry it. *Scenario
  FM-05.*

The one platform-specific guarantee:

- **Linux: the direct child cannot outlive an abruptly killed daemon.** The
  child is launched with `Pdeathsig: SIGKILL`, so `kill -9`, an OOM kill or a
  panic on the daemon kills it before its retry can overlap it. This covers the
  direct child only; a grandchild the integration spawned is reparented and is
  covered only by the graceful group paths. *Scenario FM-03.*

### 6.1 macOS abrupt-death exposure

*Explicit non-guarantee.*

Darwin has no parent-death signal: `syscall.SysProcAttr` has no `Pdeathsig`
field and XNU has no equivalent of `PR_SET_PDEATHSIG`. After an abrupt daemon
death on macOS the in-flight child is reparented and keeps running, while
startup marks its attempt failed and retries it. The same work can therefore run
twice, concurrently, and an integration whose external effects are not
idempotent can be duplicated. Closing this needs a supervisor process or a
death-watch pipe inside the child; persisting a process group is not enough,
because a startup sweep is post-crash cleanup and cannot portably distinguish a
live child from a recycled pid.

This is documented as a limitation, not fixed, and is tracked as `OT-009`
(closed) in [open-work.md](open-work.md) and in
[architecture.md](architecture.md#child-lifetime-is-platform-specific).

## Appendix A — the WS4 fault matrix and its evidence

WS4 names the scenarios the runtime's failure behavior is judged against, in
[the `v0.2.0` release plan](v0.2.0-release-plan.md#new-capability-the-fault-matrix).
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
| FM-01 | SIGKILL the daemon mid-run; every interrupted run reaches a terminal state or is re-enqueued; >50 at once | **Simulated** | `TestCrashRecoveryMarksRunningRunsFailedAndRetries`, `TestCrashRecoveryHandlesMoreThanOneListingPage` (240 seeded rows), `TestCrashRecoveryWithoutRetryPolicyLeavesRunFailed`, `TestStartupFailClosedOnIncompleteRecovery`, `TestReconcileQueueReenqueuesMoreThanOneListingPage`. Admission/refusal: `TestSubmitRunRejectsUnknownAndInvalidIntegrations`, `TestUnreleasedIntegrationIsRefused`, `TestDrainingRuntimeRejectsNewRuns`, `TestPausedWebhookIsUnavailableButManualRunsStillWork`. A real SIGKILL, and a submit→close→reopen→read durability test: none. |
| FM-02 | SIGKILL the daemon between finish and retry; no terminal failure without its retry | **Simulated** | `TestFinishRunPersistsOutcomeAndSuccessorAtomically`, `TestFinishCommitFailureLeavesNoPartialOutcome`, `TestFallbackJournalIsAppliedOnRestart`, `TestUnreadableFallbackJournalDoesNotReRunRunningRuns` |
| FM-03 | Kill the child, leave the daemon; attempt recorded, descendants do not survive | **Partial** | Linux direct child: `TestChildDiesWhenDaemonIsKilled` (real SIGKILL of a stand-in, no DB). In-process child death: `TestTimeoutMarksRunTimedOut`, `TestTimeoutIsRetriedWhenPolicyAllows`. Attempt-recorded-under-a-real-kill: none. |
| FM-04 | Crash during retry backoff; retry claimable after `available_at`, backoff preserved | **Partial** | `TestClaimRespectsAvailableAt`, `TestRetryPolicyRetriesUntilSuccess`, `TestClaimedRunIsRequeuedWhenMarkRunningFails`. A real restart mid-backoff: none. |
| FM-05 | Cancel a running run; process group dies, status `cancelled`, not retried | **Partial** | `TestCancelRunningRunIsNotRetried`, `TestCancelQueuedRunRemovesItFromTheQueue`. Process-group death under cancel: none. |
| FM-06 | Two processes claim concurrently; no run executed twice | **Simulated** | `TestClaimConcurrentNoDoubleExecution` (8 goroutines, one `MaxOpenConns(1)` connection). A second process or connection: none. |
| FM-07 | `concurrency: N` state updates match documented behavior | **Partial** | Parallelism observed: `TestConcurrencyLimitSerializesRuns`, `TestConcurrencyAboveOneRunsInParallel`, `TestWorkerPoolLimitCapsGlobalConcurrency`, `TestCapacityReserveRelease`, `TestClaimCapacityIsPerIntegration`. State: `TestConcurrentWriters` (distinct keys), `TestStateEndpoints`, `TestStateWriteRefusesAStaleGeneration`, `TestStateAPIRoundTripAndNamespacing`. A same-key read-modify-write race: none. |
| FM-08 | Disk exhaustion / oversized output; failures visible, bounded, recoverable | **Partial** | Bounded output/input: `TestRunTruncatesVeryLongLines` (`internal/executor`), `TestRequestBodyTooLarge`. Disk exhaustion (ENOSPC) and recovery: none. |
| FM-09 | The attempt state machine admits only legal transitions; a terminal attempt is never resurrected | **Partial** | Classification: `TestStatusSemantics`, `TestCreateRejectsAnInvalidStatus`, `TestSetStatus`. Guards: `TestMarkRunningOnlyTransitionsClaimableRuns` (terminal and `running` not claimable), `TestFinishRecordsOutcome`, `TestFinishTxJoinsTheCallersTransaction` (non-terminal finish rejected). A terminal row being overwritten: none — the store primitive is unguarded (see the caveat in §1). |

### What Appendix A means for the guarantees

Every guarantee in §§1–6 cites at least one scenario that has at least a
Partial test, and the untested half of a Partial scenario is written as an
explicit non-guarantee rather than a promise. No scenario below is yet at the
strength the release plan intends:

- **FM-01, FM-02, FM-06** need a harness that kills the daemon and the child
  for real, and a second connection or process for the claim race. Simulated
  coverage is real coverage of the recovery logic, but it cannot detect a bug
  that only a genuine SIGKILL exposes.
- **FM-03, FM-04, FM-05, FM-08** need the real crash/kill halves of the
  scenario.
- **FM-07** needs a same-key read-modify-write race to turn the
  [§4.1](#41-there-is-no-compare-and-swap) warning into a measured result.
- **FM-09** needs a guard in the store, or a test, that a terminal row cannot be
  overwritten.

When those land, this appendix moves from Partial/Simulated to Real and the
contract version increments.

## Appendix B — where this fits

| Question | Document |
| --- | --- |
| How the runtime is built and why | [architecture.md](architecture.md) |
| Every `otter.yaml` field, including `concurrency` and `retry` | [manifest-reference.md](manifest-reference.md) |
| Every endpoint, credential and error code | [api-reference.md](api-reference.md) |
| Operating guidance, capacity tuning and troubleshooting | [operations.md](operations.md) |
| The work this contract is the evidence for | [v0.2.0-release-plan.md](v0.2.0-release-plan.md) |
| Phase gates and the freeze release | [product-roadmap.md](product-roadmap.md) |
| Known open defects and unscheduled work | [open-work.md](open-work.md) |
