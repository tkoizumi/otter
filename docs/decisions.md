# Decisions

Status: living record. Started 2026-10-01.

This log records implementation decisions — the ones that change a design or a
runtime contract and would otherwise be re-derived next session. Roadmap and
sequencing decisions are recorded in
[product-roadmap.md](product-roadmap.md#recorded-decisions); the design docs carry
the detail.

Format: date, the decision, why, and what it changes. If a design doc and this log
disagree, this log wins until it is updated.

## 2026-10-01 — Self-hosted runtime API, UTC schedules

**Committed path.** The runtime API is the product surface for a self-hosted,
single-tenant deployment. `CL-22` (dynamic schedules) and `CL-23` (a reachable,
scoped API) are pulled forward of the Phase B gate. The roadmap entry is
[product-roadmap.md](product-roadmap.md#recorded-decisions).

| Decision | Answer | Consequence |
| --- | --- | --- |
| Reachability | **The operator decides how the runtime API is reached.** Otter Cloud is another client of the same API, with no privileged path. | `CL-04`'s outbound channel becomes one deployment choice rather than the only model. Both modes speak one API contract, so nothing needs a second surface. |
| Supported remote default | Private network + scoped token | `otter deploy` stops refusing a non-loopback bind; `assert-host-permissions.sh` must assert configured posture, so `P0-07`'s recorded host evidence is **re-run**, not amended. |
| Track order | Self-hosted first; Cloud as a later wrapper over the same API | `CL-22` and `CL-23` belong to the Phase 2 "Operable runtime" track, where `v0.4.0` already promises the API stops moving. |
| Schedule time zone | **Pin UTC everywhere** | A behaviour change: `scheduler.New` builds `robfig/cron` without `WithLocation`, so existing manifests currently mean the **host's local time**. The migration documents it; the design carries a per-row `timezone` column for the future. |
| Manifest schedules | One `trigger.cron` per job for the first cut; an `id:`-keyed `schedules:` list later | Keeps reconciliation trivial and the first cut behaviour-identical — that is the parity gate. No schema dependency: `schedules.origin_ref` holds a constant now and an id later, with a one-line backfill. |
| Castor job code | Out of scope for now | The `P0-13` interim shim — tick job, database-as-queue, status mirror — is **cancelled** ([plan](p0-13-castor-import-implementation-plan.md)). The durable Castor fixes (lease, single-active-run index, cancel, shared import core) still stand, because they improve the Lambda path too. |

**Why the reachability answer matters.** Loopback was standing in for
authorization, and treating the platform as privileged would have re-created the
same problem in a different place. One API, one credential model, reachability as
a deployment choice.

**Still open**, tracked in
[dynamic-schedules-design.md](dynamic-schedules-design.md#14-open-questions): token
scope granularity, TLS termination approach, `schedule_fires` retention, and
whether schedule-level pause is exposed.
