# Recovery truncation implementation plan

Status: implemented. Date: 2026-09-28. Target release: `v0.1.19`. Shipped in
commit `ffb5940` ("Fix default limit of 50 runs for recovery"), including the
`>50` regression test.

The fail-closed startup hold this document originally bundled with the read fix
was deliberately deferred and is defect 6 of the
[v0.2.0 release plan](v0.2.0-release-plan.md). That is the resolution of the
scheduling conflict: `v0.1.19` carries the truncation fix only, and the startup
policy lands in `v0.2.0`.

## The defect

Three daemon paths ask for every run in a status, and all three silently receive
at most 50.

```go
stale, err := d.runs.ListByStatus(ctx, runs.StatusRunning, 10000)   // recovery.go:23
list, err := d.runs.ListByStatus(ctx, status, 10000)                // recovery.go:74
list, err := d.runs.ListByStatus(ctx, status, 10000)                // daemon.go:714
```

`ListByStatus` delegates to `List` (`runs.go:365-367`), and `List` treats any
limit outside `(0, 1000]` as "caller did not specify", substituting the listing
default (`runs.go:338-341`):

```go
limit := f.Limit
if limit <= 0 || limit > 1000 {
    limit = 50          // silent substitution, no error, no warning
}
```

`Ascending: true` orders by `created_at ASC, rowid ASC` (`runs.go:331-333`), so
the rows returned are the **oldest** matches. A caller asking for 10,000 rows
receives the 50 oldest and a nil error.

### Affected callers

| Caller | Statuses | Consequence of truncation |
| --- | --- | --- |
| `recoverRuns` (`recovery.go:22-64`) | `running` | Runs beyond the first 50 stay `running` forever: no queue row, no terminal state, no retry successor |
| `reconcileQueue` (`recovery.go:70-102`) | `queued`, `retrying` | Orphaned runs beyond the first 50 keep no queue row and are never claimed |
| `cancelRunsOfRemoved` (`daemon.go:702-746`) | `queued`, `retrying` | After a reload removes an integration, runs beyond the first 50 are never cancelled, keep their queue rows, and later execute from a release the operator intended to retire |

The third caller has a consequence the first two do not: the work is not merely
stranded, it is **executed after the operator asked for it to stop**.

## Worked example

A workspace has 200 runs in `running` when the daemon is killed with `kill -9`.
This is reachable whenever the queue is deep and workers are busy: a
backlogged integration, `concurrency` raised above 1, or several integrations
executing at once.

**Startup 1.** `recoverRuns` executes before triggers and workers start
(`daemon.go:280-285`).

| | |
| --- | --- |
| Rows requested | 10000 |
| Rows returned | 50 — the oldest by `created_at` |
| Log emitted | `recovering_interrupted_runs count=50` |
| Outcome | Those 50 are marked `failed`, logged, and retried per policy |

The log reports `count=50` and is indistinguishable from a healthy recovery of
50 runs. Nothing records that 150 rows were skipped.

**Runs 151-200 are now stranded.** Their status is still `running`, but:

- `recoverRuns` did not visit them.
- `reconcileQueue` scans only `queued` and `retrying` (`recovery.go:73`), so it
  never sees a `running` row.
- Both functions run only inside `New`, before triggers are registered
  (`daemon.go:280-285`). There is no periodic reconciliation.

A stranded `running` run is invisible to the worker pool (no `run_queue` row),
never reaches a terminal state, and never produces a retry successor.

**Startup 2.** The 50 recovered at startup 1 are now `failed`, so the query
returns the next 50 oldest. Runs 151-200 recover; runs 51-150 remain stranded.

Recovery therefore advances exactly 50 runs per restart. Because long uptime is
the product's purpose, a host that runs for weeks holds 150 non-terminal runs
for weeks, and those runs are lost outright if the host is replaced.

## Why this is not merely a missing feature

The failure is silent and self-concealing:

1. The out-of-range limit is a **fallback**, not a rejection, so no call site
   can observe it.
2. The recovery **log reports the truncated count as the total**, so the
   operator sees a plausible number rather than a shortfall.
3. No existing test exceeds the threshold. Recovery is tested by seeding one or
   two `running` rows directly (`daemon_test.go:946-969`), so the bug is
   invisible to the suite.

A larger literal would not fix it. `1000` is also a ceiling, and the offset
paging that would be needed to reach past it has no stable tiebreaker for rows
sharing `created_at`. This is the reason the
[host recovery plan](host-recovery-implementation-plan.md) §7 requires a keyset
cursor on `(created_at, id)`, and the reason this change belongs in `v0.1.19`
rather than being papered over and revisited.

## Design

Two distinct kinds of read are currently conflated:

- **Bounded display reads** — "the newest 20 runs", "the last 5 for this
  integration". A default is appropriate, and `List`'s coercion is harmless.
- **Exhaustive semantic reads** — "every run in this status", used by recovery
  and reconciliation. Completeness is the entire point; a default is a defect.

The fix separates them rather than changing one number.

1. **Keep `List` and `ListByStatus` as bounded reads.** Their callers are
   display paths (`cli.go:436,538,951`, `api/server.go:874`,
   `runs.go:477`) and a default is the correct behaviour there. This change does
   not alter their behaviour.

2. **Add an exhaustive, keyset-paginated read** for the three daemon callers. It
   streams or collects every matching row with no default ceiling, paging until
   exhaustion. Ordering and the cursor share one key, so no row is skipped or
   processed twice.

3. **Migrate the three callers** in `recovery.go` and `daemon.go` to the new
   read.

4. **Make the silent substitution observable.** `List`'s coercion stays for
   compatibility, but the out-of-range case should be documented on the function
   and, when a limit was supplied and rejected, logged at debug level. The point
   is that the next reader of `runs.go:338-341` should not have to discover this
   by accident.

### Cursor semantics

Timestamps are stored as fixed-width UTC strings
(`2006-01-02T15:04:05.000000000Z07:00`), so lexicographic order matches
chronological order and the comparison is valid in SQL. The cursor is the pair
`(created_at, rowid)`, matching the existing ascending order:

```sql
SELECT <columns> FROM runs
 WHERE status = ?
   AND (created_at > :after_created_at
        OR (created_at = :after_created_at AND rowid > :after_rowid))
 ORDER BY created_at ASC, rowid ASC
 LIMIT :page_size
```

Page until a page returns fewer than `page_size` rows. `rowid` is used rather
than `id` because the existing ascending order already breaks ties with `rowid`;
using a different tiebreaker than the ordering is how rows get skipped.

**Mutation safety.** `recoverRuns` changes `status` while iterating, and
`reconcileQueue` enqueues rows while iterating. Neither mutates `created_at` or
`rowid`, and each underlying query re-reads the table rather than holding an
open cursor, so the key is stable across pages. The cursor must nevertheless be
advanced from the **last row of the previous page**, not from a row's
post-mutation state.

`cancelRunsOfRemoved` deletes queue rows, not run rows, so its cursor is stable
by construction.

### Rejected alternatives

- **Raise the ceiling to a larger number.** Moves the cliff and leaves the
  silent-substitution footgun. A workspace with more runs than the new ceiling
  reintroduces the same bug.
- **Make `List` return an error for an out-of-range limit.** Correct in
  principle, but it changes shared behaviour for display callers in the same
  release as a correctness fix. Deferred; noted in Follow-ups.
- **Offset-based paging with `LIMIT`/`OFFSET`.** No stable tiebreaker for rows
  sharing `created_at`; rows can be skipped or double-processed as the set
  mutates. This is what host recovery §7 explicitly rejects.

## Implementation steps

1. Add the exhaustive read to `internal/runs` (for example
   `ListByStatusAll`) with keyset pagination over `(created_at, rowid)`. Keep it
   internal to the store; callers receive every matching run.
2. Add a store test that seeds more rows than one page and asserts every row is
   returned exactly once, in order, across page boundaries. Include rows sharing
   an identical `created_at` to exercise the tiebreaker.
3. Migrate `recoverRuns` (`recovery.go:23`) and `reconcileQueue`
   (`recovery.go:74`) to the new read.
4. Migrate `cancelRunsOfRemoved` (`daemon.go:714`).
5. Document the coercion on `List`, and log the rejected-limit case at debug
   level.
6. Keep the existing bounded callers unchanged, and add no limit to the three
   migrated call sites.

## Tests

**The load-bearing test, written first.** Seed **more than 50** `running` runs —
200 is a reasonable stand-in for a backlog — construct the daemon over that data
directory, and assert:

- Every seeded run reaches a terminal state; none remains `running`.
- The recovery log reports the true count, not the page size.
- Runs whose attempt count permits a retry have exactly one successor attempt;
  runs at the attempt limit do not.

This test must **fail against current `main`** before any fix lands. If it
passes before the change, it is not exercising the defect.

**Supporting tests.**

- `cancelRunsOfRemoved` with more than 50 queued runs for a removed
  integration: every one is `cancelled` and has no queue row.
- `reconcileQueue` with more than 50 orphaned `queued`/`retrying` rows: every
  one is re-enqueued exactly once.
- Pagination boundary: a page ending mid-tie (several rows sharing
  `created_at`) does not skip or duplicate.
- Page size of 1, to prove the loop terminates and covers the set.
- Zero matching rows, and a count that is an exact multiple of the page size.

## Acceptance

- The >50 regression test fails before the change and passes after.
- No row in `running`, `queued`, or `retrying` is silently omitted by any daemon
  startup or reload path.
- No schema migration, so `v0.1.19` downgrades to `v0.1.18` by swapping the
  binary.
- `make test` and `make smoke` pass.
- The three migrated call sites contain no magic row limit.

## Scope and follow-ups

**In scope.** The exhaustive read, the three callers, and the tests above.

**Out of scope.** The atomic finish/retry transaction, child process lifetime,
the fail-closed startup hold, and the fault matrix. Those are defects 4-6 of the
[v0.2.0 release plan](v0.2.0-release-plan.md); the fail-closed hold (defect 6)
lands in `v0.2.0`, not here.

**Follow-ups.**

- Consider making `List` reject an out-of-range limit rather than substituting a
  default, once the display callers are audited.
- The same silent-default pattern should be checked anywhere else a limit is
  coerced; this plan addresses `runs` only.
- `reconcileQueue` resets `available_at` to now (`recovery.go:188`), discarding
  retry backoff. That is a separate defect and is not fixed here.

## Evidence status

The defect and the affected call sites were verified by reading the source at
`v0.1.18`. It has since been reproduced and fixed: the `>50` recovery regression
test (`TestCrashRecoveryHandlesMoreThanOneListingPage`), the pagination tests in
`internal/runs`, and the reconciliation tests pass, so this document's claim is a
runtime fact rather than an inspection.
