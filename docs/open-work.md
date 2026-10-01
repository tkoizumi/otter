# Open work

Status: living document. Created: 2026-09-28. Last reviewed: 2026-09-28.

Tasks raised in working sessions that are not yet captured by a release plan or
fixed in code. This is the intake list, not a schedule: items move to a release
plan when they are scheduled, and disappear from here when they ship.

Sorted by what is being worked on next: `v0.2.0` at the top, then each later
version in order. Within a version, items are ordered by dependency, so an item
that constrains another comes first.

## How to use this

- **One home per task.** If it is scheduled in
  [v0.2.0-release-plan.md](v0.2.0-release-plan.md) or
  [product-roadmap.md](product-roadmap.md), it lives there and not here. This
  document holds what those do not cover yet.
- **Verify before trusting.** Every item records how it was found. An item
  marked *source* has been read in the code but not reproduced at runtime. An
  item marked *doc* is a documentation defect, verifiable by reading the doc
  against the schema or code.
- **Graduate to GitHub when scope grows.** File an issue when an item crosses
  more than one release, needs design discussion, or should be visible to
  someone who is not reading this repository. Until then, keep it here.
- **Keep the ID stable.** `OT-00N` is a durable reference for commit messages.
  Moving or rescheduling an item does not renumber it; a retired ID is not
  reused.

Status values: `open`, `scheduled` (a release plan names it), `blocked`,
`done`.

## Tasks

### v0.2.0 — next

The bulk of `v0.2.0` is sequenced in
[v0.2.0-release-plan.md](v0.2.0-release-plan.md). What appears here is only the
work for that version that the release plan does not yet cover.

None open. `OT-009` (state the macOS orphan-child limitation) closed with the
child-lifetime fix: `Pdeathsig` on Linux, and the macOS exposure stated in
[architecture.md](architecture.md#child-lifetime-is-platform-specific). The
limitation is documented, not fixed; a macOS watchdog remains unscheduled.

The WS7 reproducible-releases audit raised `OT-011` below. It does not gate
`v0.2.0`: the runtime contract states the retry half as an explicit
non-guarantee rather than an unproven promise, so the published contract still
only guarantees what a matrix scenario exercises.

### v0.3.0

Scheduled in [v0.3.0-release-plan.md](v0.3.0-release-plan.md), which turns the
items below into workstreams and adds the operating-drill evidence the roadmap's
exit gate requires. This table remains their intake record.

| ID | Task | Kind | Evidence | Status |
| --- | --- | --- | --- | --- |
| OT-001 | Fix documented retention SQL that filters on `runs.queued_at` | doc | `operations.md:507,529,549`; column is `created_at` (`migrations/0001_init.sql:28`) | open |
| OT-006 | Automatic retention for run logs and runs | feature | `runs.LogStore.DeleteOlderThan` exists (`runs/logs.go:280`) but is called only from a test | scheduled |
| OT-004 | Expose queue age, per-job depth, and last-success freshness | feature | `/health` reports counts only (`api/server.go:353-391`); no `created_at` age anywhere | open |
| OT-005 | Ship a `migration_applied` log line, or stop promising one | doc | promised at `operations.md:698`; `Migrate` has no logger and `schema_migrations` has no description column | open |
| OT-008 | Document backlog behavior and its consequences | doc | [see below](#backlog-behavior-to-document) | open |
| OT-011 | Prove queued and retry attempts retain their bound release and environment | test | closed by P0-02: `TestRetryExecutesTheParentsReleaseSnapshot`, `TestRetryResolvesTheParentsManagedEnvironment`, `TestPendingBacklogKeepsItsDigestThroughAReleasePrune`, `TestRecoveryPlansRetriesFromTheBoundRelease` | done |

The retention work was ordered by dependency: the documented SQL was wrong
before retention existed, retention had to understand pinning before it pruned,
and nothing in the group was safe to ship until `OT-002` was settled.

`OT-002` and `OT-003` are done. Every prune path now runs only with a readable
pin set, and refuses rather than treating an unreadable registry as "nothing is
pinned"; the deploy release step passes a keep window (default 3), so a deploy
converges the release directory instead of leaving every snapshot on disk. See
[v0.3.0-release-plan.md](v0.3.0-release-plan.md#ws2--release-retention-and-deploy-pruning).

#### Reproducible releases — closed

`OT-011` is done. The retry half is now proven and the contract states it as a
guarantee: a retry executes its parent's bound snapshot, under managed Python it
resolves its parent's prepared environment, a pending backlog keeps its digest
through a prune, and crash recovery plans the successor's policy from the bound
release in both directions (a looser live policy must not extend a bound run's
budget, a stricter one must not withhold the retry it promises). See
[runtime-contract.md §5.1](runtime-contract.md#51-retries-use-the-bound-release-and-environment).

#### Phase 0 findings — found while verifying P0-01…P0-04

Raised by the independent verifiers. These are the early entries in the ranked
gap list P0-18 produces: none blocks the cutover, and each is a real behaviour
or documentation boundary that was previously unstated. Ordered by how much a
paying operator would care.

| ID | Task | Kind | Evidence | Status |
| --- | --- | --- | --- | --- |
| OT-012 | A transiently unreadable release snapshot strands an interrupted run's retry | design | `internal/daemon/recovery.go` `recoveryRetryPolicy`; demonstrated by the P0-02 verifier: fixed code gives a chain of 1, the live-policy fallback gave a chain of 2 whose attempt 2 succeeded. Stated in `runtime-contract.md` §5.1 | open |
| OT-013 | A restore to a different data directory needs manual repointing of two absolute paths | design | `internal/release/stage.go` (absolute symlink), `internal/pyenv/manager.go` (absolute `interpreter`). A relative activation symlink and an interpreter derived from the env dir would remove the step; documented as a restore step today | open |
| OT-014 | Recovery's intent to never run work twice is not delivered for a kill before the journal write | design | The FM-02 harness records 2 completion side effects per finish-window kill; a pre-commit journal would close it. `recovery.go`'s comment no longer overstates it | open |
| OT-015 | The unit's resource caps are unit-wide, not per-job | design | `internal/deploy/render.go`; a per-job cgroup is what would make "the runaway job dies, the daemon does not" literal rather than a consequence of `OOMPolicy=continue` | open |
| OT-016 | `requirePython` skips rather than fails, so a runner without `python3` silently drops job-execution evidence | test | `internal/daemon/daemon_test.go`; CI now pins Python 3.13, so the hole is closed there but the skip remains | open |
| OT-017 | The backup/restore drill never runs the case where the original data directory still exists | test | `scripts/drill/backup-restore.sh` moves the original away, so silent resolution against the old directory is untested; the drill's own `release_source_dir` assertion would catch it | open |
| OT-018 | The prune test copies the CLI's pin query instead of exercising it | test | `internal/daemon/retry_binding_test.go` (import cycle keeps `internal/cli` out); the CLI's own test covers that entry point | open |
| OT-019 | The backup drill does not cover the secrets file or the pinned binary | test | `docs/operations.md` §Backups lists both; the drill covers database, releases, environments, tools and job sources | open |
| OT-020 | A run whose captured output cannot be written still reports **succeeded**, and no queryable surface signals the loss | design | Found by the HW-6b disk-fill on the real host: with 10 MiB free, a job emitting 64 MiB produced repeated `run_log_write_failed` events (`runs: commit log batch: database or disk is full (13)`) at level error, while the run finished `status=succeeded, exit_code=0` and `/health` stayed `ok`. The job's exit code is honest, but its output is gone and only the journal says so. Evidence: `docs/evidence/phase-0/2026-09-30-p0-11-host-hw6b-disk-fill.txt`. A per-run log-loss flag surfaced through the API would make it visible where operators look | open |
| OT-021 | The uv-cache fallback does not fire when the cache directory exists but cannot be written | defect | Found deploying the rebuilt host from a file-sandboxed agent: `uvCachePath` (`internal/deploy/uv.go`) falls back to the deploy's scratch space only when `os.MkdirAll` on `~/Library/Caches/otter/uv` *fails*, but on an existing directory it returns nil — the sandbox then refuses the later `os.MkdirTemp` inside it, and the deploy dies with `mkdir .../uv/.uv-download-…: operation not permitted`. That is precisely the read-only cache location the comment claims to tolerate. `OTTER_UV_CACHE` pointed inside the workspace is the workaround; a write probe (or falling back on any error from the cache path) would make the fallback real | open |

#### Backlog behavior to document

Detail for `OT-008`. Raised while answering "what happens if a run exceeds its
next scheduled run?" The answer is worth writing down, because it runs opposite
to intuition:

- Every cron occurrence becomes its own run, unconditionally. Nothing is
  skipped because a previous execution is still going.
- With the default `concurrency: 1` the runs serialize: a slow job
  builds a catch-up backlog rather than overlapping.
- **Downtime skips; slowness accumulates.** Occurrences missed while the daemon
  was down are never replayed, but occurrences during uptime always queue.
- A deep backlog executes the release bound at *submission*, not the newest
  release, so queued work can run older code than what is active.
- Raising `concurrency` to clear a backlog produces genuinely concurrent
  executions of the same job.

The first three belong in [operations.md](operations.md) near the existing
capacity section. The release-binding point belongs with the release docs.

### v0.4.0

| ID | Task | Kind | Evidence | Status |
| --- | --- | --- | --- | --- |
| OT-010 | Daemon-side release pinning check before retention | gap | pinning lives only in the CLI path (`cli/release.go:645-685`); a submit can race retention | open |

### v0.5.0

| ID | Task | Kind | Evidence | Status |
| --- | --- | --- | --- | --- |
| OT-007 | Decide the missed-occurrence policy for a busy job | design | `cronTick` enqueues unconditionally (`daemon.go:750-771`); admission has no depth bound | open |
| OT-012 | Tracked references: answer "which runs touched this order/file?" | feature | [tracked-references-implementation-plan.md](tracked-references-implementation-plan.md) | scheduled |
| OT-022 | The monitoring installer can never report a converged host, and its suite is green because it stubs the probe | defect | Found by running the installer against the live host rather than its fixture. The probe (`scripts/install-monitoring.sh` lines 355-371) tests `/etc/otter/heartbeat.env` and `/etc/otter/disk-check.env` with a plain `[ -f ]` as the ssh user, but `/etc/otter` is `drwxr-x--- root root`: the files exist and the units load them, and the probe cannot see them, so it reports `(missing)` on every run and the `already installed and converged` branch is unreachable. `scripts/test-install-monitoring.sh` cannot catch it because its fixture intercepts the whole probe (`match *# install-monitoring-probe*`) and returns whatever the case wants, so the probe's logic never executes and the converged case asserts only how the installer handles an empty result. Fix: read those two files under privilege, and stop stubbing the probe wholesale so the permission-sensitive lines run against a fixture that models a `0750` directory the login cannot read | open |
| OT-023 | No drill covers failure notification (P0-09) | test | `scripts/drill/` holds backup-restore, deploy-failure, disk-pressure, liveness and unit-caps; there is no notification drill, so `make drill` never exercises the path P0-09 is about. Its only host evidence is an ad-hoc window script that lives outside both repos. P0-10 and P0-11 each have a committed, sabotaged drill; P0-09 should too (a stand-in receiver, a failing run, a succeeding run, and an `OTTER_NOTIFY_ON` exclusion) | open |
| OT-024 | The CloudWatch metric contract is asserted nowhere across the two repos | design | The publisher hardcodes `Namespace=Otter` and the dimension name `Host` and takes `Heartbeat`/`DiskCheck` from its callers; `otter-platform` declares `MONITORING_NAMESPACE`, `MONITORING_DIMENSION`, `HEARTBEAT_METRIC` and `DISK_CHECK_METRIC` independently. They agree today, checked by hand against live AWS, but nothing ties them: a rename on either side leaves the alarm firing forever (or never) and no test says why. A generated constant, or a check that reads the live alarm and compares it to the publisher's call sites, would close it | open |
| OT-025 | The host-window harness is not in the repo | tooling | `phase-0-host-run-plan.md` §7 proposed `scripts/drill-host.sh` (precondition -> run -> assert -> transcript -> restore, refusing without `OTTER_HOST` and a window lock). It was never built; each window's procedure instead lives in one uncommitted script under `.otter-keys/`. The transcripts are complete enough to re-derive a procedure, but the harness is not reproducible from the repo, and lane B's discipline depends on whoever holds the host remembering it | open |
| OT-026 | Retention is not configured on the live host, and nothing re-applies it on a rebuild | defect | Found by the 2026-10-01 independent verification of P0-09/P0-10/P0-11 (D1). The live daemon logs `retention_configured log_retention=0s run_retention=0s` on every start and `capture_configured retention=168h0m0s`; the deployed daemon env file carries `OTTER_WORKERS` only, and there is no unit drop-in and no `retention:` key in `otter.deploy.yaml`. The ledger's HW-6a record ("left configured `OTTER_RUN_RETENTION=2160h` / `OTTER_LOG_RETENTION=720h`") was made on host `44.213.227.52`, destroyed 2026-09-30; the current host was rebuilt afterwards and the rebuild procedure in `phase-0-resume-here.md` does not mention retention. So `CA-33`'s "define retention before enabling unattended jobs" is not in force on the host that would run Castor's job. Closing it needs a deploy-time retention key (or a unit drop-in) plus a rebuild step, and the same care for capture retention, which still has no configuration surface at all. Evidence: [independent record](evidence/phase-0/2026-10-01-p0-09-p0-11-independent-verification.txt) | open |
| OT-027 | The monitoring suites stay green with the behaviour the alarms depend on deleted | test | Found by the 2026-10-01 independent verification (D2) and an independent sub-audit. Confirmed: (a) deleting `--aws-sigv4`, `--user` and the session-token header from `scripts/otter-metric.sh:186-188` leaves `scripts/test-otter-metric.sh` at 29/29 — the stand-in records no headers and accepts any request, so the signature that makes the metric land is asserted nowhere; (b) publishing `AnythingIsFine=1` instead of `DiskCheck=1 DiskFreeMB=…` at `scripts/disk-check.sh:233` leaves `test-disk-check.sh` at 18/18 **and** `scripts/drill/disk-pressure.sh` green, so a rename of the metric the P0-11 alarm watches ships undetected; (c) `heartbeat.sh` is executed by no *suite*: neutering its publish left 165 suite cases green — but the committed `scripts/drill/liveness.sh` **does** catch it (exit 1, "the check did not publish for a healthy runtime"), so the gap is that `make test` and CI never run the drills rather than that the script is untested; (d) `assert_log` is an unanchored substring match, so `Otter`→`OtterMetricsX`, `StorageResolution` 60→600, `Host`→`HostName` and value extensions all pass; (e) the disk suite's `df` tap ignores its arguments, so measuring `/` instead of the data filesystem passes. The production code is correct as deployed and the real API path was re-derived on the host; it is the fixtures that prove less than the "verified by mutation" language implies. The shell suites are also not wired into `make test` or CI. Evidence: [independent record](evidence/phase-0/2026-10-01-p0-09-p0-11-independent-verification.txt) | open |

`OT-007` is the item most likely to graduate to a GitHub issue: it is a product
promise about work preservation, not an implementation detail, and the default
must not silently drop occurrences for event-driven jobs.

#### Tracked references

Detail for `OT-012`. Raised by the question "a Shopify order or an audio file
came through the job; which run contained it?" -- asked from the artifact
backwards, while every existing command starts from a run id. The reverse index
is designed in
[tracked-references-implementation-plan.md](tracked-references-implementation-plan.md).

- **What exists.** The webhook trigger body is already persisted into
  `runs.metadata` (`internal/daemon/view.go:326`), and captured HTTP bodies are
  already sanitized and stored (`internal/inspection/store.go:286`). Both hold
  the identifiers that answer the question; neither is indexed or queryable.
  `otter request <request-id>` (`migrations/0006_http_exchanges_request_id.sql`)
  is the existing precedent for a lookup over a non-unique value.
- **What is missing.** A `run_refs` index written at the two ingest points, an
  explicit `ctx.track()` for what payloads cannot see (a local audio file), an
  `otter track <ref>` read, and the cleanup and retention coupling that keeps the
  index from outliving its runs.
- **Why it is not in `v0.3.0`.** That plan's stated goal is
  [no migration ships](v0.3.0-release-plan.md); a queryable index is a new table,
  and no `v0.3.0` exit-gate step fails without it. It is also not `v0.4.0`, which
  freezes the manifest, SDK, CLI JSON and HTTP API rather than adding surface.
  `v0.5.0` is the first release that can carry a new manifest key and command
  family without contradicting the release it lands in.
- **Exit.** A webhook-triggered run whose body carries an order id is findable by
  that id, a run that declared a local file path is findable by it, a value that
  was never recorded is distinguished from one that was, and deleting a
  job leaves no reference that still resolves.

## Where these came from

The defects and doc errors above were found by a read-only source audit of
`v0.1.18` performed on 2026-09-28, plus the question about schedule overrun.
The audit did not execute anything: findings marked *source* are read from code
and still need a failing test before any fix is trusted. Findings marked *doc*
are verifiable by reading the document against the schema or code, and those are
confirmed.

## Not tracked here

- Work already sequenced in [v0.2.0-release-plan.md](v0.2.0-release-plan.md).
- Phase-level outcomes and gates in [product-roadmap.md](product-roadmap.md).
- Implementation designs in the per-feature
  `*-implementation-plan.md` documents.
