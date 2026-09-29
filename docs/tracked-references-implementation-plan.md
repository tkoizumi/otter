# Tracked references: implementation plan

Status: proposed. Date: 2026-09-29. Intake: `OT-012`. Target release: `v0.5.0`.

## Objective

Answer the operator question *"which runs touched this order, or this file?"*
from the artifact's side. The acceptance scenario is a Shopify order id that
arrived in a webhook body: after the job misbehaves, the operator runs
`otter track 4242` and gets the runs that handled it, without having edited the
job and without grepping `runs.metadata` by hand.

The second acceptance scenario is an audio file. It may or may not cross the
network, so the plan must work in two modes: identifiers Otter can observe in a
payload, and identifiers only the job knows.

Today every command starts from a run id. This plan adds the reverse direction:
a durable, indexed `run_refs` record, written when the information already flows
past the daemon, plus an explicit SDK call for the part that does not.

## Scope and decisions

Ship:

- A `run_refs` table with `(run_id, kind, value, normalized, source)` and the
  indexes both read directions need.
- Automatic extraction at the two points where identifiers already arrive:
  the trigger body at submission, and sanitized HTTP exchanges at capture
  ingestion.
- A per-job `track:` manifest section that declares what to extract and
  how each kind of value is normalized.
- `ctx.track(kind, value)` in the Python SDK and an endpoint it calls, for
  identifiers no payload contains (a local file path, a value derived mid-run).
- `otter track <ref>` to read the index, and the run's own references surfaced by
  `otter run-status` and `GET /v1/runs/{id}` so an operator can pivot either way.
- Reference cleanup joined to run and job deletion, on day one.

Defer:

- Retroactive extraction. Nothing scans historical runs, and nothing backfills
  `run_refs` from `runs.metadata` or expired capture payloads. The index starts
  when the feature ships; that is stated, not worked around.
- Free-text search over reference values. `LIKE '%value%'` is a full scan and a
  different product; `otter track` matches an indexed value, never a substring.
- Cross-job joins, reference-to-reference graphs, and any attempt to
  link two references to each other.
- A browser UI, a general tracing/observability export, and OpenTelemetry.
- Tracking anything as an authorization input. References are discovery metadata
  and must never gate access to a run.

Do not rebuild `otter request <request-id>`: it already solves the same shape one
level down, and the two lookups share a design but not a code path. Do not read
`runs.metadata` at query time as a fallback; an unindexed scan returning a
partial answer is worse than an honest "not recorded".

## Version placement

This is recorded here because the first question asked of the design was whether
it belongs in `v0.3.0`, and the answer is a property of that release, not of this
feature.

`v0.3.0` states as a goal that **no migration ships**, so that upgrade and
downgrade stay a binary swap ([v0.3.0-release-plan.md](v0.3.0-release-plan.md)).
A queryable index is a new table; there is no version of this feature that
answers the question without one, because the whole defect is that
`runs.metadata` is unindexed. `v0.3.0` also does not need it: its exit gate is
driven by drills for install, diagnose, rotate, upgrade and restore, and the
diagnosis step is already served by `otter trace`, `otter logs` and
`otter requests`.

`v0.4.0` freezes the manifest, SDK, CLI JSON and HTTP API. Adding a manifest key,
a command family and an SDK method to the release whose purpose is to stop the
surface moving would invert the release order.

`v0.5.0` is the first release that can add surface without contradicting itself,
and it is the last of Phase 2, whose outcome is an operable runtime. Ship it
there, or later if `v0.5.0` fills with capacity work.

## Repository facts

Checked against the working tree on 2026-09-29. Recheck the citations before
implementation and preserve unrelated changes.

| Area | Existing behavior and implementation touchpoints |
| --- | --- |
| Unindexed trigger body | `runs.metadata` is `TEXT` with no index (`migrations/0001_init.sql:33`); the webhook body and headers are encoded into it by `encodeTriggerMetadata` (`internal/daemon/view.go:326`) and parsed back only by the SDK (`sdk/python/otter/trigger.py`). |
| Submission path | `SubmitRunWithOptions` (`internal/daemon/view.go:166`) resolves the bound release, then writes the run and its queue row in one transaction (`:301`). The trigger body is in scope as `payload.Body` before that transaction. |
| Capture ingest path | `Ingest` (`internal/inspection/store.go:286`) sanitizes each event with `SanitizeEvent`, merges it, and writes inside one transaction. It is the only place a sanitized body exists. |
| Sanitization | `internal/inspection/redact.go` and `sanitize.go` run before storage; extracted values therefore cannot disclose anything redaction removed, provided extraction reads the sanitized exchange and never the raw event. |
| Capture policy | `capture: off` records nothing and installs no instrumentation, so no HTTP extraction is possible for such a run. `metadata` records no bodies, so extraction from bodies is impossible while URL-based extraction still works. |
| Capture retention | `ExpireOlderThan` (`internal/inspection/store.go:616`) deletes exchange *rows* in bounded batches; `DeleteForRun` (`:568`) and `DeleteForJob` (`:591`) remove a run's or an identity's capture. Reference cleanup has to follow all three. |
| Run deletion | `runs.Store.DeleteByJob` (`internal/runs/runs.go:227`) hand-deletes `run_logs`, `run_queue` and `runs` in one transaction; `otter delete` reaches it through `internal/daemon/identity_api.go:156`. There is no run retention path yet (`OT-006`, WS1 of `v0.3.0`). |
| Lookup precedent | `idx_http_exchanges_request_id` (`migrations/0006_http_exchanges_request_id.sql`) is a non-unique lookup index with an explicit ambiguity error rather than a guessed match (`internal/daemon/inspection_api.go:89`). Copy that semantic, not the schema. |
| API surface | Routes are registered in `internal/api/server.go:79`; `Backend` is the daemon seam (`internal/api/backend.go`); wire types are `internal/api/types.go`; the CLI client is `internal/api/client.go`. |
| Run view | `api.RunView` embeds `*runs.Run` and adds chain fields (`internal/api/types.go:136`); `otter run-status` renders it (`internal/cli/cli.go:1021`). |
| SDK | `Context` is assembled in `sdk/python/otter/context.py:58`; `State` (`state.py`) and `Logger` (`log.py`) are the templates for a small write helper over `Client.put_json/post_json` (`sdk/python/otter/_client.py:105`). |
| List semantics | `runs.Store.List` (`internal/runs/runs.go:339`) silently substitutes a default limit, which is deliberate for display and must **not** be reused for `otter track`. `ListByStatusAll` (`:439`) is the pattern for a read that must be complete. |
| Database | One connection, immediate transactions (`internal/database/database.go`). Anything inside a transaction must use the transaction handle. |
| Migrations | Currently end at `0010_job_pause.sql`; add `0011_run_refs.sql` and never edit an applied migration. |
| Work in flight | `v0.3.0` WS1 adds run retention and WS2 adds release pruning; both will delete `runs` rows. If `run_refs` exists by then, both must delete its rows too, or `otter track` will later resolve to runs that no longer exist. |

## Data contract

One migration, `0011_run_refs.sql`:

```sql
CREATE TABLE IF NOT EXISTS run_refs (
    run_id         TEXT NOT NULL,
    job_id TEXT NOT NULL DEFAULT '',
    -- Namespaced artifact type, e.g. shopify.order, file, audio.clip.
    kind           TEXT NOT NULL,
    -- The value as extracted or typed. Kept verbatim so an operator can see
    -- what was actually recorded, and so an exact-match kind stays byte-literal.
    value          TEXT NOT NULL,
    -- The indexed form for the declared normalization of this kind. Empty means
    -- this kind is not normalized, and lookups then match on value.
    normalized     TEXT NOT NULL DEFAULT '',
    -- operator | sdk | trigger | http_request | http_response
    source         TEXT NOT NULL,
    created_at     DATETIME NOT NULL,
    PRIMARY KEY (run_id, kind, value, source)
);

-- The `otter track` read: one (kind, normalized) value to its runs.
CREATE INDEX IF NOT EXISTS idx_run_refs_lookup ON run_refs (kind, normalized, created_at DESC);
-- The reverse read: a run's own references.
CREATE INDEX IF NOT EXISTS idx_run_refs_run ON run_refs (run_id);
-- Job teardown without a join back to runs.
CREATE INDEX IF NOT EXISTS idx_run_refs_job ON run_refs (job_id);
```

`kind` is the namespace that makes a bare value unambiguous — `shopify.order`
and `file` can both hold `1234` without conflating them. `source` is what lets an
empty result be explained instead of merely reported.

Decisions this schema fixes:

- **Identity is `(kind, normalized)`, not the bare string.** `value` is retained
  for display and for `exact` matching. Normalization is per kind, declared once
  so every writer of a kind agrees; a per-rule normalization would let two rules
  disagree and silently stop matching.
- **Uniqueness is per `(run_id, kind, value, source)`.** Two attempts of the same
  job in the same run reporting the same order under `sdk` collapse to
  one row; the same value found by both trigger extraction and SDK reporting is
  two rows, and the read-only display deduplicates.
- **Rows are per attempt, not per chain.** Attempt 3 extracts from its own HTTP
  exchanges, and a retry of the same order must be visible as its own attempt.
  The read groups by chain.

## Normalization contract

Declared once per kind in the manifest and applied by every writer.

| Mode | Meaning | Indexed form |
| --- | --- | --- |
| `exact` | Byte-for-byte. The default. | `value` |
| `fold` | ASCII case-folded. | lowercased `value` |
| `basename` | The final path segment, case-folded. For files whose directories differ per host or release. | fold of `filepath.Base(value)` |

`basename` is deliberately lossy: `a/clip.mp3` and `b/clip.mp3` become the same
reference. That is a real trade — it matches a file that moved, and it also
matches two different files with one name. It is off by default, it is the
manifest author's choice per kind, and the documentation must say so rather than
present it as an improvement. A kind that needs both behaviors declares two
kinds (`file` and `file.basename`) rather than changing semantics by writer.

When mode is `exact`, `normalized` is `value`. When a value cannot be normalized
safely (empty, over the length bound, containing a NUL), the row is rejected and
counted, never stored in a form that would match something else.

## Extraction contract

Three writers. The first two require no job change, which is where the
value of this feature comes from; the third covers what payloads cannot see.

### 1. Trigger body, at submission

In `SubmitRunWithOptions` (`internal/daemon/view.go:166`), after the trigger type
is resolved and before the run/queue transaction, walk the manifest's trigger
rules against `payload.Body` (already parsed JSON) and insert reference rows
inside the same transaction as the run row. A run therefore cannot exist without
its trigger references, and a crash cannot produce one without the other.

This is the Shopify case with zero job changes: the order id is in the
body the webhook already delivered.

### 2. HTTP exchanges, at ingestion

In `inspection.Store.Ingest`, walk the request rules and response rules against
the *sanitized, merged* exchange, in the same transaction that writes it. Two
extraction sources are available without reading bodies:

- the sanitized URL, for identifiers carried in a path or query
  (`/admin/orders/4242.json`, `?file=clip.mp3`), which works under
  `capture: metadata`;
- sanitized JSON bodies, which require `capture: full`.

Extraction must use the sanitized exchange. Reading the raw event body would let
a tracked value echo a secret that redaction removed, which turns a diagnostic
index into a disclosure path.

### 3. Explicit report, from the job

`ctx.track(kind, value)` for identifiers no payload contains — the local audio
path is the motivating case. Declare-only semantics (an idempotent insert) rather
than ledger semantics: a run may re-declare freely, a duplicate changes nothing,
and no read-modify-write or sequence number is needed.

Declared values are subject to the same validation and bounds as extracted ones.
They are stored because the job said so, and the `source` field says who
said it, so a wrong declaration is attributable rather than silently trusted.

### Manifest surface

```yaml
track:
  kinds:
    shopify.order:
      normalize: exact
    file:
      normalize: basename
  trigger:
    - path: order.id
      kind: shopify.order
    - path: orders[*].id
      kind: shopify.order
  request:
    - path: order_id
      kind: shopify.order
  response:
    - path: data.orders[*].id
      kind: shopify.order
```

- `path` is a bounded dotted path with `[*]` array fan-out. No general JSONPath,
  no filters, no expressions: a path language is a compatibility surface and
  belongs in `v0.4.0`-style policy work, not in this feature.
- Rules are per direction (`trigger`, `request`, `response`) because the same
  key means different things in each.
- Every `kind` used by a rule must be declared in `kinds`. `otter validate`
  rejects an undeclared kind, an unknown `normalize`, a malformed path, and a
  duplicate kind declaration.

## Bounds

Writers must never fail a run. A run is the durable unit of work; a diagnostic
index is not worth refusing it. Every limit below is enforced by truncation or
rejection plus a counter, and the counter is surfaced rather than hidden.

| Bound | Value | On breach |
| --- | --- | --- |
| Value length | 256 bytes | Reject the value, count it. |
| Kind length | 64 bytes, `^[a-z0-9][a-z0-9._-]*$` | Reject, count it. |
| Rules per direction | 32 | Manifest validation error. |
| Values per rule per event | 100 | Stop extracting from that rule, count it. |
| References per run per source | 500 | Stop inserting, count it. |
| `ctx.track` values per run | 100 | API rejects with `429`; the SDK raises. |
| `otter track --limit` | 1–1000, default 100 | Out of range is a usage error, not a silent substitution. |

The extracted-value caps are the ones that matter: a Shopify payload containing
10,000 orders must not produce 10,000 rows. The counter is `run_capture`
material, reusing the mechanism that already keeps "incomplete capture" honest
instead of adding a second one — which means a `tracked_refs_dropped` column on
`run_capture` in the same migration, and its reporting in `otter requests` and
`otter track`.

## API contract

Add to `internal/api/{backend,types,server,client}.go`:

```text
GET  /v1/refs?kind=<kind>&value=<value>&limit=&cursor=      admin
GET  /v1/runs/{id}/refs                                     admin
POST /v1/runs/{id}/refs                                     principal (own run)
```

- The read endpoints are operator-only, matching `otter requests` and
  `otter trace`: a reference index spans jobs and must not be readable
  with a per-run token.
- The write endpoint is scoped to the caller's own run, like log append and
  capture ingest. It validates kind and value, applies the per-run caps, and
  returns the count accepted.
- The write endpoint stays open after the run is terminal. A post-terminal
  declaration is a legitimate part of a failed run's cleanup path, and it cannot
  change a terminal outcome. This is a deliberate difference from state writes,
  which refuse after termination; the plan author should confirm it against the
  run-token lifetime in `internal/daemon/workers.go` before implementing, because
  a token revoked at exit would make the endpoint unreachable in exactly the
  failure case it exists for. If the token is revoked, the SDK must flush
  declarations before exit the way capture already does.
- The read response carries `total`, `truncated`, and the matched `(kind, value)`
  set, so a caller can never mistake a page for the complete answer.

## CLI contract

```sh
otter track <value> [--kind <kind>] [--limit N] [--all] [--json] [--pretty]
otter track <value> --since <time> --status <status> --job <name>
otter track --run <run-id>              # the reverse: what this run touched
otter run . --track <kind>=<value>      # operator-declared, repeatable
otter track <value> --attach <run-id>   # operator backfill for one known case
```

- `<value>` is required and is matched on `(kind, normalized)`. `--kind` narrows
  it. A bare value that matches more than one kind is refused, exit 2, listing
  the candidate kinds — the same posture as the ambiguous request id, never a
  guessed match.
- Output groups by retry chain: one line per chain with the root run id, attempt
  count, worst status, job and last activity. `--attempts` expands to
  attempts. The operator asking "which run had this order" is almost always
  asking about the chain, not attempt 3.
- Human form on a terminal, JSON when piped or with `--json`, `--pretty`
  overriding both, following `otter requests`.
- Exit codes match the existing convention: `0` inspected (even when the run
  failed), `1` not found or runtime failure, `2` usage.
- A not-found answer explains itself and never reads as proof of absence:
  - No rows, kind declared: "no run has recorded this; the index starts when the
    job's manifest declared `track:`, and HTTP references require
    `capture: full`."
  - No rows, kind not declared: name the kinds that do exist for the job.
  - Rows exist, none for `--job`: say so, rather than reporting nothing.
- `--json` always includes `total` and `truncated`; `--all` is the explicit
  request to page to completion, and only then is the result claimed complete.
- `--attach` writes references to an existing run with `source=operator`. It must
  refuse a run whose job generation no longer matches, following the
  fence the rest of the runtime uses, so a backfill cannot attach to a retired
  identity's history.

## Run view

`RunView` gains `refs` and `refs_truncated`, shown by `otter run-status` and
returned by `GET /v1/runs/{id}`. Cap the projection at 20 references by
`created_at`, state the truncation, and keep the full read available through
`otter track --run`. This is the reverse pivot: an operator reading a failure
learns what artifacts it touched without knowing to ask.

## Cleanup and retention

The index must not outlive what it describes.

- `runs.Store.DeleteByJob` (`internal/runs/runs.go:227`) gains a
  `DELETE FROM run_refs WHERE job_id = ?` inside its existing
  transaction. This is the path `otter delete` uses.
- The `v0.3.0` WS1 run-retention path, if it ships first, must delete a run's
  references in the same transaction that deletes the run. If it ships without
  this, `run_refs` needs the same sweep and `otter track` must treat a missing
  run as a stale row rather than an error.
- Capture retention (`ExpireOlderThan`) deletes exchange rows but must **not**
  delete references extracted from them. The reference is a summary of what the
  run touched, like `run_capture` itself; deleting it when a payload expires
  would make the answer depend on the retention window. Say this in the docs: the
  capture payloads expire, the reference does not.
- `runs.Store.Get` an unknown run after a reference row exists is a stale index,
  not a bug to paper over: the read reports the row as orphaned.

## Repository surfaces to change

| File | Change |
| --- | --- |
| `migrations/0011_run_refs.sql` | New: the table, its indexes, and the `run_capture.tracked_refs_dropped` column. |
| `internal/track/` | New package: kinds, normalization, path walking, validation, the store, and the query read. |
| `internal/config/manifest.go` | `track:` section, rules, kind declarations, validation, and `otter validate` errors. |
| `internal/daemon/view.go` | Extract trigger references in `SubmitRunWithOptions`; add operator `--track` on submission. |
| `internal/inspection/store.go` | Extract request/response references in `Ingest`; count drops on the capture summary. |
| `internal/daemon/*_api.go` | `Backend` implementations for the three routes and the `RunView` projection. |
| `internal/api/{backend,types,server,client}.go` | Wire types, routes, client methods. |
| `internal/runs/runs.go` | Reference deletion in `DeleteByJob`. |
| `internal/cli/track.go` | New: `otter track` and its rendering. |
| `internal/cli/cli.go` | Dispatch, `--track` on `otter run`, `run-status` display. |
| `sdk/python/otter/track.py` | New: `ctx.track`, bounded by the client's retry policy. |
| `sdk/python/otter/context.py` | `self.track`. |
| `docs/{manifest-reference,api-reference,architecture,operations}.md` | The new key, the routes, the table, and the documented limits. |
| `docs/runtime-contract.md` | State the guarantee and its edges: what is recorded, what is not, and that absence is not proof. |

## Implementation sequence

1. **Schema and store.** Migration, `internal/track` kinds/normalization/path
   walking, insert and both queries, table-driven tests for normalization and
   the caps. No callers yet.
2. **Manifest.** `track:` parsing and `otter validate` errors. Land it separately
   so a malformed rule is rejected before any extraction runs.
3. **Trigger extraction.** The submission transaction, with a test that a run
   with no references and a run whose extraction failed are distinguishable.
4. **HTTP extraction.** `Ingest`, sanitized-input only, with a redaction test
   that proves a value redaction removed is not tracked. This is the test that
   keeps the feature from becoming a disclosure path.
5. **API and run view.** Routes, scoping, caps, the `RunView` projection.
6. **CLI.** `otter track`, the ambiguity and not-found explanations, chain
   grouping, `--json`, `--all`, `--attach`, `run-status` display.
7. **SDK.** `ctx.track`, the post-terminal token question above, and a test that
   a declaration survives a run that fails after declaring.
8. **Cleanup, docs and limits.** Deletion coupling, the four documents, the
   contract statement, and the `OT-012` retirement in
   [open-work.md](open-work.md).

## Required tests and verification

- Normalization: `exact` is byte-literal, `fold` folds ASCII only, `basename`
  matches across directories and is documented as lossy, rejected values are
  counted and not stored.
- Ambiguity: a bare value matching two kinds exits 2 and names both; `--kind`
  resolves it.
- Honest absence: not-found distinguishes never-recorded, kind-not-declared, and
  recorded-by-another-job.
- Completeness: a result past `--limit` reports `truncated` and only `--all`
  claims completeness.
- Redaction: a value removed by redaction from a request body is not tracked.
- Policy: under `capture: metadata`, URL extraction still works and body
  extraction does not; under `capture: off`, neither does, and the not-found
  answer says why.
- Bounds: a payload with 10,000 order ids produces at most the per-rule cap, and
  the drop is visible.
- Lifecycle: a job delete leaves no reference that resolves; a reference
  whose run was deleted is reported as orphaned, not as a match.
- End to end: a webhook run carrying an order id is found by `otter track`, from
  a real submission through the real ingestion path. A manifest-only unit test
  does not prove this.
- SDK: `ctx.track` before and after a mid-run failure, against a real daemon.

## Acceptance

- [ ] `otter track <order-id>` returns the webhook runs that carried it, with no
      job change beyond the manifest's `track:` section.
- [ ] A run that declares a local audio file path is findable by it.
- [ ] A value never recorded is reported as not recorded, with the reason, and is
      never presented as proof of absence.
- [ ] `otter track` over a value matching many runs reports `truncated` unless
      `--all` was given.
- [ ] A redaction test proves no tracked value discloses removed content.
- [ ] Deleting a job leaves nothing that resolves to its runs.
- [ ] Capture payload expiry does not erase references.
- [ ] `otter validate` rejects an undeclared kind, an unknown normalization, a
      malformed path, and an over-limit rule count.
- [ ] The documented limits and defaults match the enforced values.

## Out of scope

- Retroactive indexing or backfill from `runs.metadata`.
- Substring, fuzzy or full-text search.
- Reference graphs, cross-job joins, dashboards, or OTel export.
- Tracking as an authorization input.
- Run retention itself (`OT-006`, `v0.3.0` WS1); this plan only requires that
  whatever deletes runs also deletes their references.
