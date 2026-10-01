# P0-13 — Castor Shopify import on Otter: implementation plan

Status: proposed; **partly superseded** 2026-10-01. The interim scheduling shim —
the tick job, the database-as-queue restructure and the status mirror — is
cancelled by [decisions.md](decisions.md) in favour of dynamic schedules on the
runtime API. The durable Castor fixes (lease, single-active-run index, cancel,
shared import core) and the job design remain valid when Castor work resumes.
Nothing in this plan is shipped by writing this document.

This revises P0-13 ("Select and harden the job") in
[phase-0-tasks.md](phase-0-tasks.md#L273) after Castor's codebase came under the
same ownership as Otter's. It assumes "task 13" means P0-13, and that Castor may
be modified where that removes a migration dead end.

## Objective

Run one real Castor scheduled Shopify import on the dedicated Castor runtime,
with no control plane, **without losing any ingestion control Castor's UI has
today**, and with P0-13's hardening (`CA-02`, `CA-40`–`CA-46`) closed by a
destination-side assertion rather than a run status.

## 1. Scope

**In scope.** Scheduled Shopify import execution; the scheduling tick that
materializes due datasets; the Castor-side queue change that makes the executor
swappable; credential scoping for the job; shadow comparison and cutover.

**Out of scope.** Castor's control-surface Lambdas (`datasets-api`,
`connections-api`, `import-api`, Shopify OAuth) and every UI route. Metrics,
monitors and processes — the process engine and metric scheduler are later
migrations of the same shape, not part of P0-13.

**Non-goals, recorded so they do not leak in.**

- Castor calling the Otter runtime API. `CL-10` is deferred to Phase E
  ([phase-0-tasks.md](phase-0-tasks.md#L370)); Phase 0's contract is that "a
  scheduled import needs no Otter API; the integration surface is the data, not
  the API" ([cloud-alpha-readiness.md](cloud-alpha-readiness.md#L806)). The
  daemon is loopback-only and asserted that way by P0-07.
- A per-dataset or per-interval Otter cron. Schedules are manifest-only and
  change only by edit + `otter reload`
  ([manifest-reference.md](manifest-reference.md#L128)), which a UI cannot drive.
- An SQS trigger in Otter. Otter has cron, webhook and manual triggers; adding a
  queue adapter is runtime work this plan exists to avoid.
- Migrating more than one import. P0-13 proves the pattern once.

## 2. The control-surface contract

This is the part that must not regress, and the check that would catch it if it
did.

| UI action | Owner of truth | Mechanism after the migration |
| --- | --- | --- |
| Change how often | `datasets.sync_interval_minutes` | `PATCH /v1/datasets/{id}`; the job reads it next tick |
| Turn ingestion off | `datasets.sync_enabled` | Same PATCH; the job stops materializing the dataset |
| See status, rows, errors | `sync_runs`, `sync_checkpoints`, ClickHouse | `GET /v1/datasets` unchanged; the job mirrors status |
| Import a resource now | `sync_runs` queue | `POST /v1/connections/{id}/imports` enqueues; the job claims |

**The rule.** `datasets.sync_enabled`, `datasets.sync_interval_minutes`,
`datasets.next_sync_at` and `sync_runs` are the schedule and status of record.
Otter's cron expression is a heartbeat and must never encode a customer's
cadence. A repository check (a test asserting no `castor-*` manifest carries a
non-heartbeat cron, or a lint on the job directory) is part of the deliverable,
because the failure mode is silent: someone writes `*/15` in a manifest and the
UI's Off switch stops working with nothing red.

Preserved behaviour, explicitly: the interval set `{5, 15, 30, 60, 360, 720,
1440}`, `Off`, manual import, and latest-sync status/stats/error rendering.

## 3. Why the queue moves to Postgres

Castor already stores every unit of work twice: a durable `sync_runs` row and an
SQS message. `import-api.ts` inserts the row, sends SQS, and on failure marks the
row failed ([import-api.ts:139](../../castor-app/infra/lambda/import-api.ts#L139));
the sync-scheduler does the same
([sync-scheduler/handler.py:55](../../castor-app/infra/lambda/sync-scheduler/handler.py#L55)).
**Postgres is already the ledger; SQS is only the delivery channel.**

**But the queue is shared.** `import-worker` serves all three providers — Shopify,
PostgreSQL and Salesforce — from this one queue and this one table
([handler.py:159](../../castor-app/infra/lambda/import-worker/handler.py#L159)).
P0-13 migrates one Shopify import, so the collapse is partial at first: the
scheduler goes, while SQS and the worker stay for the other providers and for
manual imports. Routing becomes explicit — each dataset is served by exactly one
executor — and the lease is what enforces it.

Collapsing the two into one durable queue is the change that makes the executor
swappable:

- the Otter job claims `sync_runs` rows with `FOR UPDATE SKIP LOCKED` — the
  pattern the scheduler already uses for due datasets;
- a `claimed_by` / `lease_expires_at` pair makes Lambda and Otter mutually
  exclusive, so both can run side by side during shadow without double
  execution (`CA-40`, `CA-41`);
- cutover becomes a configuration flip rather than a rewrite, and the
  sync-scheduler Lambda is deleted rather than ported.

## 4. Target architecture

```text
Castor UI ──▶ Castor API ──▶ Postgres
                             ├── datasets   (policy: enabled, interval, next_sync_at)
                             └── sync_runs  (durable queue + status ledger)

Otter runtime (dedicated Castor host, loopback API only)
  castor-sync-tick    * * * * *   materialize due datasets ──▶ sync_runs(queued)
  castor-sync-import  * * * * *   claim queued run, import bounded pages,
                                  ctx.state checkpoint, mirror status
```

Two jobs, one table, **no cross-job triggering**. The tick is cheap SQL and
always finishes inside its minute, so it can never build a backlog (`OT-007`).
The import job is bounded so it cannot overrun its own tick either.

The alternative — one job that both schedules and imports — is simpler and
acceptable if a manual import may wait up to five minutes for pickup. The split
exists to keep manual-import latency at roughly one minute, which is what SQS
delivers today.

## 5. Castor-side changes

### 5.1 Capture the live schema first

`sync_runs` and `datasets` have **no `CREATE TABLE` in this repository**; the
control database was built out of band. Before writing the migration, capture
and commit the live schema:

```bash
pg_dump --schema-only --no-owner "$CASTOR_CONTROL_DATABASE_URL" > infra/sql/000_control_baseline.sql
```

Otherwise the migration is written blind, and the partial unique index in 5.2
cannot be validated against real data.

### 5.2 Migration

```sql
ALTER TABLE sync_runs
  ADD COLUMN IF NOT EXISTS claimed_by       text,
  ADD COLUMN IF NOT EXISTS lease_expires_at timestamptz,
  ADD COLUMN IF NOT EXISTS cancel_requested boolean NOT NULL DEFAULT false;

-- One active run per dataset. Reconcile existing duplicates before creating it:
-- the current import API inserts a queued run unconditionally.
CREATE UNIQUE INDEX IF NOT EXISTS sync_runs_one_active_per_dataset_idx
  ON sync_runs (dataset_id)
  WHERE status IN ('queued', 'running');

CREATE INDEX IF NOT EXISTS sync_runs_claimable_idx
  ON sync_runs (queued_at)
  WHERE status = 'queued';
```

The unique index is not decoration: it is what makes "Lambda and Otter cannot
both execute this" a database fact rather than a convention.

### 5.3 `import-api`

Stop sending SQS. The row insert becomes the enqueue, and the endpoint must
tolerate an existing active run for the dataset — return the existing
`syncRunId` (idempotent) or `409`, per the product decision. Today it inserts
unconditionally, which the unique index will reject.

### 5.4 Retire the scheduler Lambda

Delete `SyncScheduler`, its EventBridge rule and its SQS grants. `schedulersEnabled`
already gates the rule ([castor-infra-stack.ts:217](../../castor-app/infra/lib/castor-infra-stack.ts#L217)),
so this is reversible during shadow. The tick job replaces both the rule and the
handler.

### 5.5 Status-mirror contract

The UI reads `sync_runs`. One **logical sync** is one `sync_runs` row, and it may
span several Otter runs. The job must therefore guarantee exactly one terminal
state per row, including when an Otter run is killed, times out, or is retried —
Otter retries are *new* runs with `parent_run_id` set
([manifest-reference.md](manifest-reference.md#L204)). A row left `running`
forever is both a UI lie and, with the claim query, a wedged dataset.

### 5.6 Cancel, if the UI should get it

`cancel_requested` is checked between pages; a cancelled run ends `cancelled` and
the dataset is claimable again. This is executor-agnostic and needs no Otter API.
Shipping the UI control is optional and separate.

## 6. The Otter jobs

Recommended home: `castor-app/otter/`, deployed with `otter deploy` — the shared
import core is then versioned with the APIs and UI it belongs to. A separate repo
also works; the `path: ../lib/python` layout is what matters.

```text
castor-app/otter/
├── otter.deploy.yaml
├── lib/python/castor_ingest/      # shared core, used by Otter and (during shadow) the Lambda
└── jobs/
    ├── castor-sync-tick/
    └── castor-sync-import/
```

### 6.1 `castor-sync-tick/otter.yaml`

```yaml
version: 1
name: castor-sync-tick
description: Materialize due Castor datasets into the sync_runs queue.

entrypoint: main.py

python:
  mode: managed
  path:
    - ../lib/python

# Cheap SQL only. The tick must always finish inside its own minute so it can
# never build a backlog (OT-007).
trigger:
  cron: "* * * * *"
timeout: 30
concurrency: 1

retry:
  attempts: 3
  backoff: exponential
  initial_delay: 2s
  max_delay: 20s

env:
  CASTOR_WORKSPACE_ID: replace-me

# A DSN, not the Lambda's JSON secret: the daemon injects this as an environment
# variable and the core parses it. Never a value in `env` -- `otter inspect`
# prints those.
secrets:
  - POSTGRES_DSN
```

Responsibilities: select due active datasets without an active run (the same
`FOR UPDATE SKIP LOCKED` query as
[sync-scheduler/handler.py:29](../../castor-app/infra/lambda/sync-scheduler/handler.py#L29)),
insert `sync_runs(..., 'queued', 'scheduled')`, advance `next_sync_at`, and
reclaim runs whose lease has expired. It performs no vendor I/O.

### 6.2 `castor-sync-import/otter.yaml`

```yaml
version: 1
name: castor-sync-import
description: Execute queued Castor sync runs, one bounded unit per run.

entrypoint: main.py

python:
  mode: managed
  path:
    - ../lib/python

trigger:
  cron: "* * * * *"

# Bounded so a run cannot overrun the next tick: the job stops at
# RUN_BUDGET_SECONDS with a resumable checkpoint instead of being killed
# mid-page.
timeout: 55
concurrency: 1

retry:
  attempts: 3
  backoff: exponential
  initial_delay: 10s
  max_delay: 45s

env:
  CASTOR_WORKSPACE_ID: replace-me
  RUN_BUDGET_SECONDS: "40"
  SHOPIFY_API_VERSION: "2026-07"
  TOKEN_KEY_ARN: replace-me

secrets:
  - POSTGRES_DSN
  - CLICKHOUSE_JSON
  - SHOPIFY_CLIENT_ID
  - SHOPIFY_CLIENT_SECRET
```

### 6.3 Claim and lease protocol

```sql
SELECT id, workspace_id, connection_id, dataset_id, resource, trigger_type
FROM sync_runs
WHERE status = 'queued'
   OR (status = 'running' AND lease_expires_at < now())
ORDER BY queued_at
LIMIT 1
FOR UPDATE SKIP LOCKED;
```

On claim: `status='running'`, `claimed_by=<run id>`,
`lease_expires_at = now() + 90s`, `started_at = coalesce(started_at, now())`.
Renew the lease each page. On exit with work remaining, set `lease_expires_at =
now()` so the next tick resumes it; on drain, `status='succeeded'`; on a fatal
error, `status='failed'` with `error_message`. A stale lease is reclaimable,
which removes the existing wedge where a killed worker blocks a dataset forever.

### 6.4 Context state

`ctx.state` is a per-job JSON key/value store with keys capped at 128 characters
([state.go:29](../internal/state/state.go#L29)); dataset-scoped keys fit
comfortably.

| Key | Value |
| --- | --- |
| `ds:<datasetId>:sync_run` | the logical `sync_runs` id being executed |
| `ds:<datasetId>:phase` | `orders` \| `products` \| `variants` \| `complete` |
| `ds:<datasetId>:cursor` | the GraphQL page cursor to resume from |
| `ds:<datasetId>:watermark` | optional `updated_at` lower bound (see below) |

This is the change that makes `ctx.state` load-bearing: today the cursor lives in
`sync_checkpoints` keyed by `sync_run_id`, so a new run restarts from page one
and relies on upsert for correctness. Dataset-scoped state resumes across runs.
Keep writing `sync_runs.stats` so the UI still shows progress.

**Watermark, optional and second.** A faithful port is "full scan with a
resumable cursor, idempotent upsert". Bounding the rescan with an
`updated_at:>=` GraphQL filter plus a small overlap — the pattern in
[otter_connectors/checkpoint.py](../../otter_examples/shopify_integrations/lib/python/otter_connectors/checkpoint.py)
— reduces work and makes the watermark real. It changes the query and needs its
own verification against the pinned Shopify API version, so land it second.

### 6.5 Hardening mapping

| Requirement | Mechanism | Assertion |
| --- | --- | --- |
| Upsert by stable external id (`CA-40`) | `ReplacingMergeTree(synced_at)`, keyed by `shopify_order_id` | Destination key set equals source key set |
| Idempotency key from business input (`CA-41`) | Business key only; the run id never enters the destination | Re-running a page changes `synced_at`, not the key set |
| `concurrency: 1` around checkpoints (`CA-42`) | Manifest `concurrency: 1` plus the lease and unique index | Concurrent claim returns no second run |
| Bounded work units (`CA-43`) | One page per unit, `RUN_BUDGET_SECONDS` per run | A run killed mid-page resumes without loss |
| Reconcile before replay (`CA-44`) | Reads are replay-safe; destination writes are key-idempotent; the lease is conditional SQL | Kill mid-page, re-run, count keys |
| Bounded retries and backoff (`CA-45`) | Manifest `retry`, plus the existing Shopify throttle handling (`429`, `5xx`, cost `restoreRate`) | Retry storm test |
| Verify business outcome (`CA-46`) | Destination assertion after each drain | Duplicate and missing-key count is zero |

## 7. Credentials

Two entanglements, one of which the runtime must not absorb.

- **Secrets Manager → daemon env.** `POSTGRES_*`, `CLICKHOUSE_*` and the Shopify
  OAuth client move to `secrets:` in the manifests and the daemon environment.
  This is the same mechanism P0-12 verified.
- **KMS stays.** Shopify access tokens are encrypted per connection with an
  encryption context of `{shop, workspaceId}`
  ([handler.py:1090](../../castor-app/infra/lambda/import-worker/handler.py#L1090)).
  Two options: grant the runtime's instance role `kms:Encrypt`/`kms:Decrypt` on
  that one key, conditioned on the encryption context, via
  [castor-runtime-stack.ts](../../otter-platform/lib/castor-runtime-stack.ts); or
  move token handling behind a Castor endpoint.

**Recommendation: narrow KMS for Phase 0.** It is one IAM policy on a stack we
own, it keeps the secret out of the job, and the endpoint alternative adds an
outbound dependency and an auth story that belongs with the Phase B control
plane.

## 8. Work breakdown

| # | Step | Size | Repo |
| --- | --- | --- | --- |
| 1 | Capture and commit the control-DB baseline schema | S | castor-app |
| 2 | Migration: lease, cancel, unique active-run index | S | castor-app |
| 3 | `import-api`: DB-as-queue, idempotent manual import | S | castor-app |
| 4 | Extract `castor_ingest` core; Lambda becomes a thin adapter | M | castor-app |
| 5 | `castor-sync-tick` job + manifest | S | castor-app |
| 6 | `castor-sync-import` job + manifest, `ctx.state` checkpoints | L | castor-app |
| 7 | KMS policy on the runtime instance role | S | otter-platform |
| 8 | Destination assertion harness | M | castor-app |
| 9 | Shadow run beside the Lambda, diff explained (P0-14) | elapsed | both |
| 10 | Cutover: delete scheduler Lambda + rule; keep the worker for PostgreSQL, Salesforce and manual imports until those migrate (P0-15) | S | castor-app |

Steps 1–3 are independent of Otter and can start as soon as the job owner picks
the import and the resource. Steps 5–6 need P0-12's credential values.

## 9. Evidence

A destination-side assertion, not a run status.

```sql
-- Exactly once, after the run has drained. ReplacingMergeTree merges lazily, so
-- dedupe explicitly before counting.
SELECT count() AS duplicate_keys
FROM (
    SELECT shopify_order_id
    FROM castor.shopify_orders FINAL
    WHERE workspace_id = {ws:UUID} AND dataset_id = {ds:UUID}
    GROUP BY shopify_order_id
    HAVING count() > 1
);
```

Plus:

- **Crash/kill resume.** Kill the import run mid-page; the next tick resumes from
  `ctx.state`; the destination key set is unchanged and duplicate-free.
- **Control surface.** Set an interval through `PATCH /v1/datasets/{id}` and see
  it honoured; set `sync_enabled=false` and observe no new `sync_runs`; queue a
  manual import and observe pickup within a tick.
- **Stale lease reclaim.** Kill a run leaving `running` with an expired lease;
  the next tick reclaims and resumes it.
- **Single executor.** With both Lambda and Otter enabled, one run executes and
  the other claim returns nothing.
- **Shadow diff (P0-14).** Keyed `FINAL` row sets compared against the Lambda's,
  with every difference explained.
- **Cutover (P0-15).** The scheduled path has no EventBridge rule and no SQS
  consumer.

## 10. Risks and open decisions

- **Backlog (`OT-007`, open).** Occurrences enqueue unconditionally; only a
  paused job skips. Mitigated by short ticks, bounded import runs, and an
  idempotent claim. If Castor wants coalescing rather than backlog, that is a
  runtime change, not a job change.
- **Manual-import latency.** One-minute pickup is the reason for the two-job
  split. Accepting up to five minutes allows a single job.
- **`ctx.state` growth.** Keys accumulate per dataset; define cleanup on dataset
  deletion.
- **Lazy dedup.** Any assertion that counts without `FINAL` will report phantom
  duplicates.
- **Two writers during shadow.** Safe only because of the lease and the unique
  index; test the claim race, do not assume it.
- **Resource choice.** `orders` is bounded by Shopify's 60-day `read_orders`
  scope, has a stable business key, and is externally verifiable. `products` +
  `variants` is a heavier full scan and a worse first target.
- **Base schema is uncommitted.** Step 1 is a prerequisite, not a nicety.

## 11. Human decisions required

1. Which import and resource — recommended Shopify `orders` — and its owner.
2. The business effect and replay behaviour, in writing (`CA-02`).
3. Castor's credential names and values (HW-5).
4. Manual import when one is already active: return the existing run, or `409`.
5. Whether the UI ships cancel in this pass.

## 12. What this removes from Castor, and what it does not

At P0-13's scope — one Shopify import — the removed surface is small and precise.

| Removed at cutover | Kept |
| --- | --- |
| `SyncScheduler` Lambda (1 of 11) | `ImportWorker` — it also serves PostgreSQL and Salesforce |
| `SyncSchedulerRule` EventBridge rule (1 of 3) | The SQS queue and DLQ, for the other providers |
| SQS send on the scheduled Shopify path | `import-api`, `datasets-api`, every UI route |
| The stale-`running` wedge (replaced by a lease) | `sync_runs` / `sync_checkpoints` — now also the queue |
| — | KMS, the secrets, Postgres, ClickHouse |

**Do not read this as "Castor stops using Lambda."** Ten Lambdas and two of the
three scheduled rules stay. The worker can only be retired once PostgreSQL and
Salesforce ingestion move too, which is explicitly outside this plan.

**The primary beneficiary is Otter.** Phase 0 exists to "absorb the risk a paying
client would otherwise absorb" ([cloud-alpha-readiness.md](cloud-alpha-readiness.md#L787)),
and every blocker is a runtime-correctness item rather than a Castor complaint.
Castor's own gains are real but modest, and three of them need no migration at
all:

- the lease, the single-active-run index and cancel are Castor fixes the new
  queue makes natural ([§5.2](#52-migration));
- one shared import core instead of Lambda-coupled code — but only once the
  Lambda path is deleted, not while both run in shadow;
- escaping the 15-minute Lambda ceiling and the Docker-based dependency bundling
  in CDK — real, but only if the import actually exceeds them;
- consolidating the sync, metric and process schedulers onto one runtime with
  one run history — the actual payoff, and it means repeating this pattern twice
  more.

Castor pays for those with a host to patch, monitor, back up and restore; split
observability between Otter's run history and `sync_runs`; a claim/lease protocol
that exists only because Otter has no dynamic schedules; a pre-1.0 runtime pinned
at `v0.3.0-rc1` on one host with no HA or autoscaling; and a second deployment
pipeline alongside CDK. At this volume the host is also a net cost increase over
the Lambdas it displaces.

The migration is worth doing when "what is painful about Castor's Lambda
ingestion today?" has a concrete answer — the 15-minute ceiling, DLQ archaeology,
three separate schedulers, or a control plane Castor actually wants. If nothing
is painful, this is a risk transfer from Otter's roadmap onto Castor. That is a
legitimate choice for a dogfood migration, but it should be named as one.

## Related

- [phase-0-tasks.md](phase-0-tasks.md#L273) — P0-13, the task this implements.
- [phase-0-execution-plan.md](phase-0-execution-plan.md#L367) — W3, the
  human-bound wave this sits in.
- [cloud-alpha-readiness.md](cloud-alpha-readiness.md#L787) — Phase 0's scope and
  the `CL-10` deferral.
- [runtime-contract.md](runtime-contract.md#L168) — cron, delivery and duplicate
  semantics the job is designed against.
- [otter-platform/README.md](../../otter-platform/README.md#L30) — host sizing and
  the `OTTER_WORKERS=1` requirement.
