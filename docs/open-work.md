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

| ID | Task | Kind | Evidence | Status |
| --- | --- | --- | --- | --- |
| OT-001 | Fix documented retention SQL that filters on `runs.queued_at` | doc | `operations.md:507,529,549`; column is `created_at` (`migrations/0001_init.sql:28`) | open |
| OT-002 | Retention must account for backlog-pinned release snapshots | design | `cli/release.go:645-685` pins digests held by `queued`/`running`/`retrying` runs | open |
| OT-003 | `otter deploy` never prunes releases; `--keep` is opt-in | gap | `cli/release.go:397-409`; deploy passes no keep value | open |
| OT-006 | Automatic retention for run logs and runs | feature | `runs.LogStore.DeleteOlderThan` exists (`runs/logs.go:280`) but is called only from a test | scheduled |
| OT-004 | Expose queue age, per-integration depth, and last-success freshness | feature | `/health` reports counts only (`api/server.go:353-391`); no `created_at` age anywhere | open |
| OT-005 | Ship a `migration_applied` log line, or stop promising one | doc | promised at `operations.md:698`; `Migrate` has no logger and `schema_migrations` has no description column | open |
| OT-008 | Document backlog behavior and its consequences | doc | [see below](#backlog-behavior-to-document) | open |
| OT-011 | Prove queued and retry attempts retain their bound release and environment | test | [see below](#reproducible-releases-not-yet-proven) | open |

`OT-001` through `OT-006` are ordered by dependency: the documented SQL is wrong
before retention exists, retention must understand pinning before it prunes, and
nothing in the group is safe to ship until `OT-002` is settled.

#### Reproducible releases not yet proven

Detail for `OT-011`. Raised by the WS7 audit against the roadmap's
[Reproducible releases](product-roadmap.md#v020--phase-1-dependable-execution)
bullet ("queued runs and retries retain their bound code and environment").

- **What is proven.** A queued run executes its bound release snapshot, not the
  live tree (`TestRunExecutesTheActiveReleaseNotTheLiveTree`). Retention protects
  releases bound to non-terminal runs (`TestPinnedReleasesIncludesOnlyNonTerminalRuns`,
  `TestRetainKeepsTheActiveAndReferenced`).
- **What is not.** No test asserts that a *retry* re-uses its parent's
  `release_digest` / `release_source_dir`, or that a managed-Python retry
  resolves its parent's `environment_digest`. No test references
  `ReleaseSourceDir` at all. `planRetry` copies the fields
  (`internal/daemon/workers.go`), so the behavior is implemented, but nothing
  exercises it end to end.
- **Why it was missed.** The contract cited `FM-02` for the retry-binding
  guarantee, but `FM-02`'s anchor tests prove only that the terminal outcome and
  its successor are committed atomically. The citation was corrected when the
  gap was found; the retry binding is now an explicit non-guarantee in
  [runtime-contract.md](runtime-contract.md#5-retries-and-external-effects).
- **Exit.** A matrix scenario that activates a newer release between a failed
  attempt and its retry, and asserts the retry ran the parent's snapshot (and,
  for managed Python, the parent's environment), plus a retention case that a
  pending backlog keeps its digest.

#### Backlog behavior to document

Detail for `OT-008`. Raised while answering "what happens if a run exceeds its
next scheduled run?" The answer is worth writing down, because it runs opposite
to intuition:

- Every cron occurrence becomes its own run, unconditionally. Nothing is
  skipped because a previous execution is still going.
- With the default `concurrency: 1` the runs serialize: a slow integration
  builds a catch-up backlog rather than overlapping.
- **Downtime skips; slowness accumulates.** Occurrences missed while the daemon
  was down are never replayed, but occurrences during uptime always queue.
- A deep backlog executes the release bound at *submission*, not the newest
  release, so queued work can run older code than what is active.
- Raising `concurrency` to clear a backlog produces genuinely concurrent
  executions of the same integration.

The first three belong in [operations.md](operations.md) near the existing
capacity section. The release-binding point belongs with the release docs.

### v0.4.0

| ID | Task | Kind | Evidence | Status |
| --- | --- | --- | --- | --- |
| OT-010 | Daemon-side release pinning check before retention | gap | pinning lives only in the CLI path (`cli/release.go:645-685`); a submit can race retention | open |

### v0.5.0

| ID | Task | Kind | Evidence | Status |
| --- | --- | --- | --- | --- |
| OT-007 | Decide the missed-occurrence policy for a busy integration | design | `cronTick` enqueues unconditionally (`daemon.go:750-771`); admission has no depth bound | open |

`OT-007` is the item most likely to graduate to a GitHub issue: it is a product
promise about work preservation, not an implementation detail, and the default
must not silently drop occurrences for event-driven integrations.

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
