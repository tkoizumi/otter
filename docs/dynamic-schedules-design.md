# Dynamic schedules: design

Status: shipped through phase 4. Date: 2026-10-01. Amended 2026-10-02: the
first cut shipped in `v0.3.0` (migration `0012_job_schedules` — one row per job,
the store-backed scheduler, and `PUT`/`DELETE /v1/jobs/{id}/schedule`).
Amended again for `v0.4.0` workstream `R2`
([v0.4.0-and-cloud-phase-b-plan.md](v0.4.0-and-cloud-phase-b-plan.md)):
migration `0014_dynamic_schedules` replaces that table with an id-keyed
`schedules` table plus the `schedule_fires` ledger; the manifest reconciles as
`origin = 'manifest'` rows that refuse API mutation with `409`; there are
per-occurrence payloads, `timezone` (UTC by default), create idempotency keys,
exactly-one-run-per-occurrence, and the
`otter schedule list|add|update|remove|pause|resume` CLI.

What remains is phase 5, the job configuration layer (§8) — the
`config_version` column exists so it can land without another migration — and
the `coalesce`/`catch_up` missed-occurrence policies, which are recorded as data
but not yet implemented (§11).

## Objective

Make a schedule first-class runtime state, so it can be created, changed, paused
and removed **without editing `otter.yaml` and reloading**. Manifest-declared
schedules keep working exactly as they do today.

This is the runtime half of `CL-10`: it is what lets a control plane — and
therefore Castor's UI — say "run this job for this dataset every fifteen
minutes" without a code change. It is runtime work on Phase B's critical path,
alongside `CL-21`.

The design also specifies the **job configuration layer** (§8), because a
dynamic schedule needs somewhere to point that is not the release.

## 1. Current behaviour, and why it cannot be dynamic

- The scheduler is in-memory: two maps around `robfig/cron`, reconciled from
  manifests on every daemon start and every `otter reload`
  ([scheduler.go:65](../internal/scheduler/scheduler.go#L65)).
- `Replace` deliberately leaves an unchanged expression alone, because re-adding
  it recomputes the next fire time and can skip an occurrence that was about to
  fire ([scheduler.go:60](../internal/scheduler/scheduler.go#L60)).
- Occurrences missed while the daemon was down are not replayed.
- There is **no schedules table**. The schema holds `job_state`,
  `job_instances`, `job_paths`, `job_pause`, `runs`, `run_queue`, `run_logs`,
  `run_capture`, `http_exchanges`, `webhook_tokens` and the identity tables —
  nothing that stores a schedule.
- A cron occurrence carries `scheduled_at` and no body:
  `api.TriggerPayload` has `Body` and `ScheduledAt`
  ([types.go:21](../internal/api/types.go#L21)), and the manual and webhook paths
  populate the body while the cron path sets only the timestamp.

**Consequence.** A schedule can only change by editing a released file and
reloading. There is no per-occurrence payload, so "this job, for dataset 42" is
inexpressible.

**The precedent is already in the tree.** `job_pause` is SQLite state for an
operator control, keyed by durable identity, with the comment *"It is an operator
control, not a lifecycle change"*
([0010_integration_pause.sql](../migrations/0010_integration_pause.sql)). The
operations guide refuses an `enabled:` flag in the manifest for the same reason —
"a declarative flag would let the next `otter reload` silently undo it" — and
then adds that *"recurring blackout windows would be a scheduling feature, not
this one"* ([operations.md:463](operations.md#L463)). Schedules are the third
instance of runtime-owned state, after pause and per-job webhook tokens.

## 2. Invariants

| # | Invariant |
| --- | --- |
| I1 | The runtime remains authoritative for scheduled work; no second scheduler exists in the control plane (`CL-05`). |
| I2 | Manifest-declared schedules stay declarative and reconcile on reload. A reload never silently changes or removes an operator's dynamic schedule, and an API write never fights a manifest. |
| I3 | One occurrence produces **at most one accepted run**, across restarts and across duplicate control commands (`CL-11`). |
| I4 | A schedule is bound to a job's durable identity id, never a label or a path. |
| I5 | `move` carries schedules; `reset` mints a fresh identity with none; `delete` purges them. |
| I6 | Every input a run depends on is pinned at submission — release digest, environment digest, and configuration version. |

## 3. Data model

Migration `0012_dynamic_schedules.sql`. Timestamps use the fixed-width UTC layout
established in `0001_init.sql`, so lexicographic order is chronological order.

```sql
CREATE TABLE IF NOT EXISTS schedules (
    id              TEXT PRIMARY KEY,
    job_id          TEXT NOT NULL,            -- job_instances.id, never a label or path
    cron            TEXT NOT NULL,
    timezone        TEXT NOT NULL DEFAULT 'UTC',
    payload         TEXT NOT NULL DEFAULT '{}', -- JSON; becomes ctx.trigger.body
    config_version  TEXT,                     -- pinned job configuration (see §8)
    missed_policy   TEXT NOT NULL DEFAULT 'skip', -- skip | coalesce | catch_up
    origin          TEXT NOT NULL,            -- manifest | api
    origin_ref      TEXT NOT NULL DEFAULT '', -- manifest: stable manifest id; api: caller
    paused_at       DATETIME,
    last_fired_at   DATETIME,
    idempotency_key TEXT,
    created_at      DATETIME NOT NULL,
    updated_at      DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_schedules_job ON schedules (job_id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_schedules_idempotency
    ON schedules (idempotency_key) WHERE idempotency_key IS NOT NULL;

-- The occurrence ledger. One row per fired occurrence, written in the same
-- transaction as the run and its queue row, so a crash between firing and
-- recording is impossible and a duplicate fire loses to the primary key.
CREATE TABLE IF NOT EXISTS schedule_fires (
    schedule_id   TEXT NOT NULL,
    occurrence_at DATETIME NOT NULL,
    run_id        TEXT NOT NULL,
    fired_at      DATETIME NOT NULL,
    PRIMARY KEY (schedule_id, occurrence_at)
);

-- A cron run records which schedule produced it, for correlation and pruning.
ALTER TABLE runs ADD COLUMN schedule_id TEXT;
CREATE INDEX IF NOT EXISTS idx_runs_schedule ON runs (schedule_id, created_at DESC);
```

Two notes on the shape:

- **`origin` is the load-bearing column.** It is what lets a declarative
  reconciler and an imperative API coexist without either silently undoing the
  other. Everything in §4 follows from it.
- **`paused_at`, not an `enabled` boolean.** This mirrors `job_pause`'s
  "absence means active" convention, and makes the two pause levels read the
  same way.

## 4. Ownership and reconciliation

`reload` reconciles **only `origin = 'manifest'` rows**. `origin = 'api'` rows are
never touched by it.

| Situation | Action |
| --- | --- |
| Manifest schedule, no row | Insert with `origin = 'manifest'` |
| Manifest schedule changed | Update `cron`, `payload`, `timezone`; **preserve** `paused_at` and `last_fired_at` |
| Manifest schedule removed | Delete that row only |
| API row | Never read, updated or deleted by reload |
| Job removed from disk | The existing delete path purges all its rows, exactly as it purges a pause |
| Job moved | No action — the row is keyed by identity, so it follows |
| Job reset | Fresh identity, so it starts with no schedules |

**Row identity across reloads.** Reconciliation must not churn rows when a
manifest reorders. Support an optional `id:` per schedule entry; fall back to
position only when it is absent, and treat a position-based match as a weaker
identity. Without this, moving a schedule up in the list resets its
`last_fired_at` and can skip or repeat an occurrence.

**Backwards compatibility.** `trigger.cron` remains valid and reconciles as a
single `origin = 'manifest'` row. A multi-schedule manifest form — a `schedules:`
list — is additive and can land later; Phase 1 needs only the existing field
([manifest-reference.md:82](manifest-reference.md#L82)).

**Precedence, stated plainly.** A manifest may not delete an API row and an API
call may not delete a manifest row. A job may hold N schedules from either
origin; they fire independently.

## 5. Firing semantics and idempotency

**Persist the past, derive the future.** Store `last_fired_at`; never treat a
persisted future `next_fire_at` as authoritative. On start and on reload, compute
each next occurrence from `now` and the expression. Nothing in the past is
fired, which preserves today's "missed windows are not replayed" contract *by
construction* rather than by discipline.

**Fire is one transaction.** On an occurrence, `SubmitRun` already records the
run and its queue row atomically. The schedule path adds the `schedule_fires`
row and the `last_fired_at` update to that same transaction. Therefore:

- a crash after submitting cannot double-fire, because the ledger row committed
  with the run;
- a crash before submitting leaves no run and no ledger row, so the next
  computed occurrence is simply in the future;
- a duplicate wake-up (two schedulers, a reload racing a fire) loses to the
  `(schedule_id, occurrence_at)` primary key instead of producing a second run.

Retries are unaffected: a retry is a new `runs` row referencing
`parent_run_id`, not a new occurrence, so it never touches the ledger.

**Pause is checked at admission, not by arming.** Today, pausing a job disarms
its cron entry and resuming re-arms it (`TestPauseUnarmsCronAndResumeRearms`).
With a store-backed source, the cleaner rule is to check both levels at fire
time — schedule `paused_at`, then job `job_pause` — mirroring the way `cronTick`
already re-checks the pause as "the last word on admission"
([daemon.go:766](../internal/daemon/daemon.go#L766)). This removes an
arm/disarm race and makes the two pause levels behave identically. It is a
behaviour change; the existing test expectations change with it.

**Control-command idempotency.** Creating or updating a schedule carries a
caller-derived `idempotency_key`; the unique index makes a retried command return
the existing row rather than creating a second schedule. This is `CL-11` applied
to the schedule surface.

## 6. Payloads and `ctx.trigger`

The plumbing already exists for two of the three trigger types. A manual run
accepts `{"body": …, "headers": …}` and exposes it as `ctx.trigger`
([api-reference.md:380](api-reference.md#L380)); webhooks do the same. Cron sets
only `ScheduledAt`.

The fire path sets `Body` from `schedules.payload`, so `ctx.trigger.body` looks
identical for all three trigger types and **no SDK change is required**. The run
records `schedule_id` and `scheduled_at` so a UI can correlate an occurrence with
its run.

Bound the payload (64 KiB is generous for a dataset identifier and a few
overrides) and validate it is a JSON object, so a schedule cannot be used as a
bulk data store.

Payloads are **not** secrets: they are printed by `otter inspect`, returned by
the API and visible in a UI. Secrets stay in the daemon environment or a
credential store, never in the control plane (`CL-13`).

## 7. Time zone

Today `scheduler.New` constructs `robfig/cron` without `WithLocation`, so
expressions are interpreted in the **host's local time**. That is invisible while
only an operator authors manifests, and indefensible once a UI writes
`0 3 * * *` for a customer.

**Recommendation.** Store `timezone` (IANA name) per row and construct the
schedule with `cron.WithLocation` for that zone. Default new rows to `UTC`.

This is a behaviour change for existing manifests, which today mean local time.
The options are to keep implicit-local for `origin = 'manifest'` rows and pin UTC
for API rows (consistent, but two rules), or to pin UTC for everything and
document it as a breaking change. **Decision required** (§14); the recommendation
is to pin UTC everywhere, because "whatever the host's clock was configured to"
is not a contract anyone can hold.

DST semantics follow the location: for a zone with DST, a `0 2 * * *` schedule
may skip or repeat an hour twice a year. That is the cron norm and should be
documented rather than special-cased.

## 8. Job configuration: the third layer

**Shipped 2026-10-03** as `R2` phase 5. Migration `0015_job_configs` adds
`job_configs` (immutable versions) and `job_config_current` (the mutable
pointer); `runs.config_version` records the version a run resolved at
submission; the daemon passes the pinned version to the child as `OTTER_CONFIG`
and the SDK exposes it as `ctx.config`. Two pieces of the design are
deliberately not in `v0.4.0`: configuration is not projected into named
environment variables (no manifest opt-in yet), and superseded versions are
retained rather than garbage-collected. Both are recorded in
[runtime-contract.md §7.5](runtime-contract.md#75-configuration-is-not-a-secret-store).

### The problem

`otter.yaml` `env` is part of the release, and that is correct: a manifest is
code, snapshotted for a run, which is what makes a retry execute what was
accepted. The example job therefore puts `SHOPIFY_STORE`, `SALESFORCE_OBJECT`
and `BACKFILL_FROM` in `env` — deployment values living in a release. The
consequences are release churn per tenant, one release unable to serve N
configurations, configuration changed by editing code, and no audit record for
the change.

The naive fix — read configuration live at execution — is worse: a retry could
then execute different behaviour than the run that was accepted, breaking the
guarantee `P0-02` established.

### The rule

**Every input a run depends on must be pinned at submission, whichever layer it
lives in.** Residency is a detail; unpinned mutability is the defect.

| Layer | Mutability | Pinned on the run |
| --- | --- | --- |
| Release | immutable | release digest, environment digest |
| **Configuration** | immutable versions, mutable pointer | **`config_version`** |
| Run input / trigger payload | per occurrence | `runs.metadata` |
| Secrets | out of band | never in the release or the control plane |

### Shape

```sql
CREATE TABLE IF NOT EXISTS job_configs (
    id         TEXT PRIMARY KEY,          -- immutable version id
    job_id     TEXT NOT NULL,
    values     TEXT NOT NULL,             -- JSON object
    created_at DATETIME NOT NULL,
    created_by TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS job_config_current (
    job_id         TEXT PRIMARY KEY,
    config_id      TEXT NOT NULL,         -- the version new runs pin
    updated_at     DATETIME NOT NULL
);
```

- A run records the `config_version` it resolved at submission, so a queued,
  retrying or backlogged run keeps it — the same property releases have.
- Changing configuration writes a new version and moves the pointer. Existing
  work is unaffected.
- Values reach the job as `ctx.config` and, where a manifest opts in, as
  projected environment variables. Precedence must be explicit and audited:
  manifest `env` provides defaults, configuration overrides, secrets are
  separate and never overridable.
- **Where it lives:** the runtime's SQLite, not the control plane. A runtime must
  execute without the cloud (`CA-*`), and `CL-05` keeps accepted work
  authoritative on the runtime. The control plane writes versions over the
  command channel and reads them for display; it does not become the store.

Schedules only need `config_version` to exist as a nullable column so this can
land without a second migration. **The configuration layer can ship after
schedules**; nothing in §1–§7 depends on it being implemented.

## 9. API surface

Runtime endpoints, authenticated as today. They are reachable locally, through
the control channel once `CL-21` lands, or from another host once `CL-23` makes
the API a supported remote surface (§9.1):

```
GET    /v1/jobs/{id}/schedules
POST   /v1/jobs/{id}/schedules           # idempotency-key required
GET    /v1/schedules/{schedule_id}
PATCH  /v1/schedules/{schedule_id}       # cron, timezone, payload, missed_policy
DELETE /v1/schedules/{schedule_id}
POST   /v1/schedules/{schedule_id}/pause
POST   /v1/schedules/{schedule_id}/resume
```

- A `PATCH` or `DELETE` against an `origin = 'manifest'` row returns `409`, with
  a message naming the manifest as the owner. The API cannot fight a reload.
- `POST` with an idempotency key that already exists returns the existing row and
  `changed: false`, matching how pause already behaves
  ([operations.md:455](operations.md#L455)).
- Operator CLI for the same surface: `otter schedule list|add|update|remove|pause|resume`,
  so the feature is usable before any control plane exists.

The command surface the least-privilege control credential (`CL-21`) must
include: list jobs, run job, pause/resume job, **and the schedule endpoints
above**. Today's spec lists everything except scheduling — that is the gap this
document closes.

### 9.1 The API as a remote surface

A schedule API is only useful to a remote client if the API is reachable. Today
it is loopback-only by convention, and the artifacts that govern it disagree:

| Artifact | Stance |
| --- | --- |
| Daemon ([config/daemon.go](../internal/config/daemon.go#L440)) | Non-loopback allowed, token mandatory — wildcard included |
| `otter deploy` ([deploy/target.go](../internal/deploy/target.go#L419)) | Refuses a wildcard address; plans the API as loopback with `ssh -L` |
| [assert-host-permissions.sh](../scripts/assert-host-permissions.sh#L495) | Fails any non-loopback listen address |

Loopback is substituting for authorization. The operator credential is one static
admin token with full control-plane authority — including code execution — and no
read-only or per-job variant ([security.md:197](security.md#L197)). The fix is
not to keep the API unreachable; it is to make the credential small enough that
reachability becomes an ordinary configuration choice — `CL-23`.

The deployment this is designed for: **a client's backend holds a scoped token
and calls the runtime API. A browser never does.** A runtime token in a browser is
a code-execution credential in a place that cannot keep it.

Concrete work, in the order that unblocks the schedule API:

1. **Scoped operator tokens.** Control (run, pause, schedule), read (jobs, runs,
   logs, state), admin; multiple named tokens with rotation and revocation.
   Until this lands the write API ships under the existing admin token — no worse
   than `POST /v1/jobs/{id}/runs` today, but it does not make the API safe to
   expose.
2. **`otter deploy` accepts a non-loopback bind behind an explicit opt-in.** The
   daemon already permits it; the deploy path refuses. Replace the refusal with a
   warning plus a required flag.
3. **TLS.** The daemon is a plain `http.Server` with no TLS listener. Either
   document reverse-proxy termination as the supported pattern or add direct TLS.
4. **`assert-host-permissions.sh` asserts configuration, not convention** —
   loopback, or a token with the expected bind or CIDR. This changes a `P0-07`
   guarantee with recorded evidence, so the evidence is re-run rather than
   amended.
5. **Reconcile two documents.** [security.md](security.md)'s "an API reachable
   from the internet with a static bearer token is the deployment the tool exists
   to prevent" becomes "discouraged without TLS and a scoped token"; `CL-04`'s
   "must not require inbound public access" keeps *public* as the operative word,
   since private-network inbound with a scoped credential is a normal deployment.

## 10. Interaction with existing behaviour

| Behaviour | Effect |
| --- | --- |
| `job_pause` | Suppresses every schedule of that job, checked at admission (§5) |
| `move` | Schedules follow — keyed by identity, no path in the row |
| `reset` | Fresh identity, so no schedules |
| `delete` | Schedules and fire rows purged with the identity, as `job_pause` is |
| `reload` | Reconciles manifest rows only (§4) |
| Backup / restore (`P0-03`) | Schedules ride in `otter.db`; restoring onto a host with different manifests reconciles only manifest rows |
| Releases | A schedule fires against the job's active release at fire time; the run pins the digest as usual |
| Retention | `schedule_fires` grows one row per occurrence. Prune on the runs window, but keep at least the current occurrence boundary so restart dedup still works |
| Overrun (`OT-007`) | Unchanged at first: every occurrence enqueues. §11 makes it a per-schedule choice |

## 11. Missed-occurrence policy

`OT-007` stops being an internal note the moment a user creates a schedule. The
policy becomes per-schedule data:

| `missed_policy` | Behaviour |
| --- | --- |
| `skip` (default) | Today's behaviour: occurrences during downtime are gone |
| `coalesce` | Fire once on the next wake-up if any occurrence was missed |
| `catch_up` | Fire each missed occurrence, bounded by a per-schedule maximum |

Computed at load from `last_fired_at` versus `now`. `catch_up` needs a bound or
it becomes a thundering herd after a long outage — and it interacts with the
backlog behaviour the runtime already documents, so it should ship with an
explicit queue-depth observation, not just a flag.

## 12. Security

- Schedule payloads and configuration values are **not** secrets and must never
  hold them (`CL-13`). Validate and document this at the API boundary.
- The control credential (`CL-21`) is scoped to the command surface in §9. A
  compromised control plane can create, change and remove schedules; it cannot
  read business data, logs or captures.
- Schedule writes are control actions and belong in the audit trail (`CL-19`),
  with the caller, the idempotency key and the before/after values.

## 13. Rollout

| Phase | Content | Gate |
| --- | --- | --- |
| 1 | `schedules` table, manifest reconciliation, in-memory arming sourced from the table | Existing scheduler and pause tests still pass; a reload parity test proves no behaviour change |
| 2 | Read API and `otter schedule list` | Manifest rows visible; API rows impossible to create yet |
| 3 | Write API, idempotency, `schedule_fires`, `CL-21` credential | Duplicate-command and crash-mid-fire tests |
| 4 | Payloads, timezone, `missed_policy` | Occurrence-payload test; DST boundary test |
| 5 | Configuration layer (§8) | Pinned-config retry test, mirroring `P0-02` |

Phase 1 is the deliberately boring one: it changes storage without changing
behaviour, which is what makes phases 3–4 reviewable.

## 14. Open questions

1. **Timezone default.** Pin UTC for everything (breaking, recommended) or keep
   implicit-local for manifest rows?
2. **Manifest syntax.** Optional `id:` per schedule in Phase 1, or add the
   `schedules:` list at the same time?
3. **Configuration home.** Runtime-owned (recommended, offline-capable) or
   control-plane-owned with per-run fetch?
4. **Schedule-level pause in the UI.** Is a second pause level a product feature
   or an implementation detail?
5. **`schedule_fires` retention.** Align with the runs window, or keep a fixed
   number of occurrences per schedule?
6. **Multiple schedules per job in manifests.** Phase 1 or later?

## Related

- [manifest-reference.md](manifest-reference.md#L82) — `trigger.cron` today.
- [operations.md](operations.md#L448) — pause, and why it is not declarative.
- [runtime-contract.md](runtime-contract.md#L168) — schedules, deliveries and
  duplicates.
- [open-work.md](open-work.md#L136) — `OT-007`, the missed-occurrence policy.
- [cloud-alpha-readiness.md](cloud-alpha-readiness.md#L231) — `CL-05`, `CL-11`,
  `CL-21`, and the new `CL-22` (dynamic schedules) and `CL-23` (reachable, scoped
  API).
- [security.md](security.md#L151) — the current loopback and token posture that
  §9.1 reconciles.
- [product-roadmap.md](product-roadmap.md#recorded-decisions) — the dated decision
  that pulls `CL-22` and `CL-23` forward of the Phase B gate.
- [p0-13-castor-import-implementation-plan.md](archive/p0-13-castor-import-implementation-plan.md)
  — the migration this capability would replace the interim scheduling shim with.
