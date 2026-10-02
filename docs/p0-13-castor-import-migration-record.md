# P0-13 — Castor Shopify import on Otter: migration record

Status: shipped for the three Shopify resources below. Salesforce, Postgres and
`fulfillment_inventory` are **not** migrated — see "Coverage". Recorded
2026-10-02.

This is the record of what actually runs, as against
[p0-13-castor-import-implementation-plan.md](archive/p0-13-castor-import-implementation-plan.md),
which is the plan. Three of that plan's parts were cancelled before they were
built (see "Against the plan"), so the plan no longer describes the shipped
system. Where the two disagree, this document is what is true.

## 1. What shipped

Three Otter jobs on one host, each syncing one Castor dataset.

| job | Castor dataset id | destination tables | schedule |
| --- | --- | --- | --- |
| `shopify-products-to-clickhouse` | `01623354-3556-442a-b4ad-7dfe8123b3e2` | `shopify_products`, `shopify_product_variants` | `*/15 * * * *` |
| `shopify-orders-to-clickhouse` | `d13707a7-30a6-46fa-b71c-13594d943153` | `shopify_orders` | `*/5 * * * *` |
| `shopify-customers-to-clickhouse` | `f6651490-b524-4f7f-ba9f-855f692d4c08` | `shopify_customers` | `*/5 * * * *` |

They share one tenancy: workspace `00000000-0000-0000-0000-000000000001`,
connection `cb7c18ef-cb9c-45ac-a8c3-75d719b09763`, database `castor`. Each job
carries its own dataset id in `CASTOR_DATASET_ID`, because one job instance
fills exactly one dataset.

**The schedule lives on the runtime, not in the manifest.** A manifest's
`trigger.cron` is only a job's *first* value: Otter imports it once, on first
sight of the job, and the API owns it from then on. This mattered more than it
sounds — see §5.

Every job is Python on a managed CPython 3.13.1, written against the live
Shopify Admin API rather than the deleted importer's code, except for orders,
which is a field-for-field transcription of that importer's `order_row`.

## 2. What Castor gave up

Removal commit `433f8d5`: **14 files, 1,727 deletions**.

| removed | size | what it did |
| --- | --- | --- |
| `lambda/import-worker/handler.py` | 1,205 lines | the import engine: the Shopify products/variants/orders GraphQL queries, the generic Salesforce and Postgres paths, the staging-table-and-swap into ClickHouse |
| `lambda/sync-scheduler/handler.py` | 136 lines | the cron that enqueued due imports |
| both `requirements.txt` | 3 lines | Lambda-side Python dependencies |
| `lib/castor-infra-stack.ts` | 103 lines | `ImportQueue`, `ImportDeadLetterQueue`, `ImportWorker`, `SyncScheduler`, `SyncSchedulerRule`, and two stack outputs |
| `lambda/import-api.ts` | 52 lines | the queueing half; it now creates or reuses the dataset and returns `status: "created"` |
| `test_postgres_connector.py`, `test_salesforce_connector.py` | 126 lines | worker-dependent coverage |
| datasets page | 76 lines | the "Automatic sync" selector |
| connections page | 31 lines | `syncRunId` handling that the API no longer returns |

Two UI removals are worth their own line. The datasets page's selector was
deleted because after the move **it controlled nothing**: it wrote a column the
runtime never read. The connections page's `syncRunId` handling had to go the
other way — leaving it in would have reported every successful import as a
failure, because the API stopped returning that field.

## 3. What AWS gave up

- **No SQS queues.** `cdk synth` emits 9 Lambdas and zero queues.
- **`SyncSchedulerRule` deleted** from the deployed stack. Only
  `MetricSchedulerRule` and `ProcessEngineRule` remain, both enabled — the
  deploy runs with `-c schedulersEnabled=true` for exactly that reason.
- **`CastorSandboxInfra` destroyed**, 238 resources, so there is no second
  environment tracking the same pipeline.

## 4. Architecture, before and after

Before — the pipeline was Castor's, in Castor's account:

```
EventBridge cron
  -> sync-scheduler Lambda      (decides which datasets are due)
    -> SQS import queue         (the queue is the durability)
      -> import-worker Lambda   (Shopify / Salesforce / Postgres -> staging table -> swap)
        -> ClickHouse
  and the app wrote `sync_runs`, which the UI read as truth
```

After — Castor keeps the control plane and gives up the data plane:

```
Otter cron (runtime state, editable through the API)
  -> job process (managed Python, one dataset per job)
    -> Shopify Admin API
      -> ClickHouse (ReplacingMergeTree, dedup on read with FINAL)

Castor: creates datasets, sets cadence, reads state back -- moves no rows
```

## 5. Schedule ownership, and why the UI changed

The rule this migration settled, and the one to carry into later work: **a
schedule is runtime state, and the manifest is not a second copy of it.**
`otter.yaml` carries a `trigger.cron` only as a job's first value; the API owns
it thereafter. The question that decided it — *what is the purpose of having it
in `otter.yaml` if it does not reflect the current state of the schedule?* — has
no good answer, so the manifest stopped being the place the UI reads from.

Consequences that followed from that single decision:

- The datasets page shows **Rows / Last sync / Next sync**, taken from
  ClickHouse and the runtime. `nextSyncAt` prefers the runtime's `next_run_at`,
  falling back to the mirror only when the runtime cannot be reached.
- The datasets-api updates the runtime **first** and mirrors after, so the
  runtime is the source of truth rather than an input to a local record.
- A dataset with no job behind it shows no cadence control at all, rather than a
  control that writes to nothing. This is why the "Automatic sync" selector had
  to be deleted instead of rewired.
- The three job descriptions deliberately name **no** cadence. They used to say
  "every five minutes" while the products job ran every fifteen — the same drift
  problem, in the one field a person actually reads.

## 6. Coverage

| dataset | before the migration | after |
| --- | --- | --- |
| Shopify products (+ variants) | imported by the worker | **migrated** — 18 products, 27 variants |
| Shopify orders | imported by the worker | **migrated** — 78 rows |
| Shopify customers | **never implemented** | **new** — 16 rows |
| Shopify fulfillment_inventory | **never implemented** | dark |
| Salesforce Account | imported by the worker | dark |
| Salesforce Contact | imported by the worker | dark |

Two rows of that table are corrections to the migration's own premise. The
worker's only queries were `PRODUCTS_QUERY`, `VARIANTS_QUERY` and `ORDERS_QUERY`,
and the import API's resource list offered only `orders` and `products`. So
**customers and `fulfillment_inventory` were never importable at all** — their
dataset rows existed, pointing at destination tables that did not exist and that
nothing could fill. Customers is not "migrated"; it is implemented for the first
time, and its destination table was created during this work. That also means
the pipeline being deleted had already stopped covering the Salesforce datasets
by the time it went.

## 7. The seams

These are the places where the new arrangement is held together by agreement
rather than by a check. Each is a place to look first when something is wrong.

1. **The dataset-id ↔ job-name map is hardcoded on both sides.**
   `datasets-api/otter_sync.py` maps a dataset id to a job name, and the job's
   manifest independently carries the same dataset id in `CASTOR_DATASET_ID`.
   Nothing verifies the two agree. A mismatch does not fail — it files a
   dataset's cadence, and its rows, under the wrong dataset. The map's test now
   pins all three pairs for that reason.
2. **Destination tables are created by a person.** The worker created them; jobs
   deliberately do not. A job pointed at a table that does not exist fails,
   which is the intended loud failure, but the table is an operator step that
   nothing in either repository performs. `shopify_customers`'s DDL lives in
   castor-app's `infra/README.md` because the importer never implemented it and
   it has nowhere else to live.
3. **The shared library is snapshotted per release, so it has per-job versions.**
   Each job declares `python.path: ../lib/python`, and the release **copies**
   that tree in. Deploying one job therefore ships the current library into that
   job's release only; the other jobs keep running their own snapshots until
   they are released too. Isolation — but drift, and the drift is invisible
   without inspecting each release.
4. **`sync_runs` is dead but still read.** Nothing writes it any more.
   `datasets-api` correctly reads the runtime instead and says so in a comment,
   but `processes-api/source.py` still selects `last_synced_at` and
   `latest_sync_status` from it. Those two fields are frozen at the last
   pre-migration write, so the processes page shows history that never moves
   while the datasets page shows live data.

## 8. Operational facts

| | |
| --- | --- |
| runtime host | `ubuntu@52.202.163.124`, AWS account `426714791664`, us-east-1 |
| workspace | `otter-examples-e0309b8c` |
| unit | `otterd-otter-examples-e0309b8c.service` |
| API | `127.0.0.1:7337`, bearer token in `/etc/otter/workspaces/*.env` |
| ingress | Cloudflare Tunnel `otter-robin-dev-3.castorhq.com` -> `127.0.0.1:7337`, Access service token |
| Castor | account `331262815338`, us-east-2, stack `CastorInfra` |
| runtime version | `v0.2.0-140-g32d2892-dirty` — **a local build, not a tagged release** |

## 9. Verification

What was checked, and the number that was checked:

- Each job validated and unit-tested before deploy: products 72, orders 79,
  customers 70, all passing.
- Windows verified against the live API rather than assumed: `updated_at:>'...'`
  returns 0 for a future instant and every record for a 2020 one, for both
  orders and customers.
- The order filter is `status:any AND updated_at:>'...'`, and the `status:any`
  half is load-bearing: Shopify's orders connection silently defaults to *open*
  orders. Verified live that a **misspelled** field in that filter matches
  everything rather than erroring (a bogus field returned all 78 records), which
  is how a typo would quietly turn every run into a full rescan.
- First runs landed their rows, each with distinct keys equal to its row
  count: products 18 (+ 27 variants), orders 78, customers 16.
- Customer nullability was taken from the schema, not a sample: `firstName`,
  `lastName`, `email` and `phone` are nullable, and the first run wrote 2 null
  phones and 15 null notes — which non-null columns would have turned into a
  failed run.
- The rename of the products job preserved identity `16e8d70d`, its 138 recorded
  runs and its `*/15` cadence, via `otter move` rather than a plain rename.

## 10. Pros

1. **One implementation of the transformation.** The mapping lives in the job;
   Castor holds no column list.
2. **Scheduling became runtime state**, editable through the API with no deploy.
3. **No Lambda ceiling.** A sync can run long, and per-page checkpointing
   resumes mid-window after an interruption.
4. **Retries, timeouts, run history and durable state come from the runtime**
   instead of being reimplemented per import.
5. **The staging-table-and-swap is gone.** `ReplacingMergeTree` dedups on read.
6. **Castor's dependency pain went with it** — the `clickhouse-connect` pinning
   bug that broke every import is no longer Castor's problem.
7. **Fewer moving parts in the AWS account**, and no per-invocation cost.
8. **Better operator visibility**, because the page reads live sources rather
   than a local ledger.
9. **Ingestion and its schedule survive an application deploy**, and vice versa.

## 11. Cons

1. **A new runtime to operate, outside Castor's AWS account.** The host, tunnel,
   Access policy and runtime version are Castor's dependency but not Castor's
   infrastructure-as-code.
2. **Coupling by convention** (§7.1): the map agrees with the manifests because
   someone keeps them in step, not because anything checks.
3. **Coverage regressed** (§6): the worker handled Salesforce and Postgres
   generically; the runtime covers three Shopify resources.
4. **Schema creation became a human step** (§7.2).
5. **A stale surface remains** (§7.4): the processes page.
6. **Creating a dataset no longer imports anything.** "Import resource" yields a
   dataset with no runtime behind it until a job exists and is mapped.
7. **Two release cycles to coordinate.** The rename needed both repositories and
   a deploy of each; there is no single version to pin.
8. **The runtime is not on a release** (§8), so "what is deployed" is not
   reproducible from a tag.
9. **Multi-tenancy is unproven.** One workspace per customer, with the tunnel,
   Access token and secrets set up per host by hand. This is the next thing to
   demonstrate, and the reason the runtime stays in this account rather than a
   customer's.

## 12. Against the plan

[p0-13-castor-import-implementation-plan.md](archive/p0-13-castor-import-implementation-plan.md)
proposed an interim scheduling shim that was cancelled before it was built, in
favour of dynamic schedules on the runtime API (see [decisions.md](decisions.md)):

- the **`castor-sync-tick` job** — the tick that materialized due datasets: not
  built; the runtime's own cron triggers do this.
- the **database-as-queue restructure** — the Postgres queue and its claim/lease
  protocol: not built; there is no queue to claim from.
- the **status mirror** — the runtime-to-`sync_runs` writer: not built, which is
  the direct cause of §7.4.

The plan's job design and its destination-side hardening remain valid where
Castor work resumes.

## 13. Open items

- **OT-029** ([open-work.md](open-work.md)) — a retired job whose source is
  still in the repository makes a full `otter deploy` fail, and it is undecided
  whether an implicit full deploy should exist at all.
- **The processes page's stale read** (§7.4) is a castor-app defect, not an
  Otter one, and is recorded here because it was found here.
- **Salesforce is untouched**, deliberately, pending the question of where the
  `salesforce_<object>_<uuid>` destination tables went: ~2,300 Account runs were
  recorded as succeeded and no such table exists in any database.
- Orphaned secrets survive the destroyed sandbox:
  `castor/sandbox/{postgres,clickhouse,shopify/oauth}`.
