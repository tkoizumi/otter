# Run timeline: implementation plan

## Objective

Implement `otter trace <run-id>` so an operator can read a finished attempt's
context, lifecycle, logs, and HTTP exchanges in one chronological view, then
open the relevant request for payload details. The operator should not need to
correlate separate command outputs or add logging to integration code.

This plan narrows `docs/run-timeline-feature.md` for the first release. Where the
two differ, implement this plan: completed attempts only, no live following,
trigger type without trigger payloads, and existing retention behavior. The
original feature description remains the broader proposal.

## Scope and decisions

Ship:

- One attempt per trace, including successful, failed, timed-out, and cancelled
  attempts. A finished failed parent is inspectable while its retry is running.
- Context showing integration identity/name, run ID, status, error, exit code,
  release digest, attempt, parent run ID, and a command for inspecting the retry
  chain. Do not merge other attempts into the event list.
- Existing `otter`-stream lifecycle lines, stdout/stderr, and HTTP summaries.
- Human output, JSONL, bounded pages, opaque continuation cursors, and an option
  to omit HTTP events.
- Capture policy, coverage, completeness, loss, and expiry explanations.

Defer:

- `--follow`, live updates, and guarantees of observing every HTTP phase.
- Trigger bodies and headers. Existing trigger storage is not sanitized to the
  HTTP inspection standard; displaying it requires a separate bounded redaction
  contract. Do not alter the input delivered to integrations.
- State history, replay, run comparison, automatic causal diagnosis, a browser
  UI, and SDK instrumentation changes.
- Retaining HTTP metadata after expiry. Today retention deletes exchange rows
  and keeps the run capture summary; preserve that behavior.

No new timeline table, ingestion endpoint, or writes from trace requests. Allow
an index-only migration: efficient chronological pagination is more important
than preserving the proposal's absolute prohibition on migrations.

## Repository facts

Checked against the working tree on 2026-09-24. Recheck applicable instructions
and preserve unrelated changes before implementation.

| Area | Existing behavior and implementation touchpoints |
| --- | --- |
| Run context | `internal/runs/runs.go`; `internal/daemon/view.go`; `api.RunView` already supplies retry context. Use an explicit trace projection, not the whole run object containing metadata. |
| Logs | `internal/runs/logs.go`; rows are appended with daemon timestamps. Existing list pagination orders by ID, not timestamp. |
| HTTP | `internal/inspection/store.go`; started/response/completed events merge into one mutable exchange. `occurred_at` is initialized from the first received event and preserved by subsequent merges. |
| Delivery | `sdk/python/otter/_capture.py`; background delivery can arrive after later-timestamped logs. Equal host clocks do not imply ingestion order. |
| Completion | `internal/daemon/workers.go`; the run's terminal status is written before its final lifecycle and retry-scheduling log lines. Terminal status is not an immutable snapshot boundary. |
| Retention | `inspection.Store.ExpireOlderThan` deletes exchange rows, leaving a capture summary. Logs can also be pruned. |
| Trigger input | `internal/api/server.go` and `encodeTriggerMetadata` preserve body data and most headers; do not call these sanitized payloads. |
| Rendering | `internal/cli/logformat.go` provides `splitStructured`/`renderLogLine`; `requests.go` provides terminal escaping and capture rendering. |
| API | Extend `internal/api/{backend,types,server,client}.go` and their fake backends/tests. Match inspection GET authorization. |
| Database | `internal/database/database.go` has one connection and immediate transactions. Keep read transactions short; all operations inside one must use the transaction handle. |
| Indexes | Logs and exchanges have `(run_id, id)` indexes. Migrations currently end at `0006_http_exchanges_request_id.sql`. |

## CLI contract

```sh
otter trace <run-id>
otter trace <run-id> --limit 100
otter trace <run-id> --after <cursor>
otter trace <run-id> --json
otter trace <run-id> --pretty
otter trace <run-id> --no-http
```

- One invocation prints one page, oldest first. Default limit 100; valid range
  1–1000. The limit applies to timeline events, excluding context and page records.
- Human form on a terminal; JSONL when piped or `--json` is supplied. Match the
  existing inspection convention: `--pretty` overrides both.
- Accept flags before or after the run ID using the existing argument helpers.
- Exit 0 means inspection succeeded even when the inspected run failed. Exit 1
  means API/runtime failure; exit 2 means invalid CLI usage.
- Reject `--follow` as unsupported usage (exit 2), independently of run status.
  Ordinary trace requests for nonterminal attempts fail with exit 1 / HTTP 409,
  with a status-specific hint:
  - `running`: use `otter logs <run-id> --follow` or `otter requests <run-id>`.
  - `retrying`: this is the next attempt waiting to execute, not its finished
    parent. Retry creation writes no logs to this new attempt: the scheduling
    message belongs to the previous attempt. Explain that following the retry's
    logs shows nothing until it is claimed for execution. When `parent_run_id`
    exists, print `otter trace <parent-run-id>` and
    explain that the parent has finished. If absent, fall back to `run-status`
    without inventing a parent.
  - `queued`: explain that the attempt is waiting for execution and point to
    `otter run-status <run-id>` and `otter status` for run and queue information.
    Do not promise a numeric queue position: the current API exposes queue depth,
    and claim order also depends on availability and integration capacity.
  A queued first attempt has no parent. Manual/cron/webhook submission appends
  its `run queued` lifecycle line, but there is no execution output yet. This
  submission behavior does not apply to attempts created by `scheduleRetry`.
  Do not wait or start a polling loop.
- A header and capture explanation always appear, even with no events. Distinguish
  no retained events from capture disabled, unavailable, expired, or incomplete.
- `--no-http` omits exchange queries and events, but still reports capture state
  and explicitly says HTTP events were excluded by the operator.

Human output should make these facts easy to scan:

```text
run: <run-id>   integration: example   status: failed   attempt: 1
release: <digest>   trigger: manual   parent: -
error: <recorded run error>
retry context: otter run-status <run-id>
capture: complete; coverage: urllib, httpx

TIME          KIND       DETAIL
12:00:00.000  lifecycle  run queued
12:00:00.020  lifecycle  run started ...
12:00:00.031  http       POST https://example.test/records -> 400, 8ms
                        main.py:27; completed; payloads: full
                        otter request <run-id> <request-id>
12:00:00.040  stderr     <exception output>
12:00:00.045  lifecycle  run failed ...
```

The example is illustrative, not a mandate to synthesize missing lifecycle lines.
Show full dates when a trace crosses midnight. Escape all remote-influenced
human strings, including context, messages, structured fields, IDs, and call sites.
Use valid shell quoting for executable follow-up commands; terminal escaping
alone does not make arbitrary request IDs safe shell arguments.

## Data and API contract

Add:

```text
GET /v1/runs/{id}/timeline?after=<cursor>&limit=100&include_http=true
```

Use an explicit response with `schema_version`, `run` (allowlisted context),
`capture`, `include_http`, `snapshot_at`, `events`, `has_more`, and `next_cursor`.
Return `events: []` for an empty page and a null cursor when there is no next page.

Each event has `kind`, `at`, `source`, `id`, and `run_id`:

| Kind | Source | Payload |
| --- | --- | --- |
| `lifecycle` | `run_logs` | Stream `otter`, original stored message. |
| `log` | `run_logs` | Stream `stdout` or `stderr`, original stored message. |
| `http` | `http_exchanges` | Request ID, method, sanitized URL, status, transport-error class, duration, phase, completeness, call site, `ingested_at`, and `updated_at`. |

Trigger type belongs in context, not a synthetic event. Do not fabricate an
event ID or timestamp for it. Run status and error in context remain authoritative
even if the terminal log is absent or has been pruned.

Preserve the raw log message in the API and JSONL. Reuse the existing structured
log parser for human rendering; do not introduce a second parsing interpretation
or move CLI dependencies into the server. No HTTP headers, request/response bodies,
trigger payloads, or raw run metadata enter the timeline response or cursor.

JSONL is a documented typed stream: one `type: "context"` record, followed by
`type: "event"` records, followed by one `type: "page"` record containing
`has_more` and `next_cursor`. Include the schema version in the context record.
This keeps capture state and continuation information machine-readable even for
an empty trace. Human warnings never appear as unstructured text on JSONL stdout.

Authorization and errors:

- Operator/admin authorization, identical to inspection GETs; reject run tokens
  even for their own run. Preserve existing loopback behavior.
- Unknown/deleted run: 404. Malformed query or cursor: 400. Nonterminal attempt:
  409. Changed evidence on continuation: 409 with a stable error message telling
  the operator to restart without `--after`.
- A cursor for another run or another `include_http` setting is invalid (400).
  Decode only after authorization and bound cursor length (for example 4 KiB).
- Return normal API error envelopes. Do not silently restart or return a partial
  success when a source read fails.

## Chronology and consistent pagination

### Event order

Use `(at, source_rank, id)`, with fixed ranks `run_logs=0` and
`http_exchanges=1`. Lifecycle and ordinary logs share the same source rank and
ID sequence. Use `database.FormatTime` for SQL bounds so fractional timestamps
compare consistently. HTTP `at` is `occurred_at`; log `at` is `timestamp`.

An HTTP line is a summary placed at its first recorded occurrence, usually its
start. Its displayed response status and duration are the latest retained values;
they were not necessarily known at that timestamp. It is not a response event.
If start capture was dropped, the first retained event can be later than the
actual request start. Keep completeness warnings visible.

Producer and daemon timestamps are approximate chronology, not causality. Log
buffering and capture delivery can reorder observation even on one host. Include
HTTP ingestion time, flag `ingested_at < occurred_at`, and document that absence
of that flag does not prove exact ordering. Never label the nearest failed request
as the cause of an exception or every HTTP 4xx as a run failure.

### Page consistency

Do not assume terminal attempts are frozen. Late lifecycle lines, in-flight
capture ingestion, and retention can change a trace between pages. Use a
stateless revision-bound cursor: continuation succeeds against unchanged evidence
or explicitly fails; it never claims to preserve a historical database snapshot.

For every page, use one short database transaction to read context, capture state,
the evidence revision, and bounded source queries. Finish the transaction before
rendering or writing to the network. Never call outer-DB store methods from inside
the transaction; add transaction-aware read helpers where needed.

Define the revision as a deterministic digest of:

- The selected run context and capture summary.
- Log row count and maximum log ID for this run. Logs are append-only except for
  deletion; together these detect supported appends and pruning without loading
  message contents. Arbitrary external edits to database rows are unsupported.
- A canonical digest of the retained HTTP summary projection, ordered by row ID,
  including update times. Current capture bounds this to 1,000 requests per run.
  Never read body/header columns to calculate this digest. Skip this component
  with `include_http=false`.

Capture summary changes still invalidate `--no-http` continuation because that
summary is part of the displayed context. The log count requires scanning the
run's covering index; measure this cost for large runs rather than claiming the
entire operation is constant time. The HTTP digest is bounded, though it is more
work than the page query alone.

Encode cursor version, run ID, HTTP inclusion setting, evidence revision, and
last emitted ordering tuple in URL-safe base64 JSON. Treat it as an untrusted
continuation token, not an authorization mechanism or a secret. Validate types,
version, timestamp, source rank, and ID. A page limit may change on continuation.

Query each included source for at most `limit + 1` rows after the tuple, using
the full lexicographic comparison:

```text
(timestamp, source_rank, id) > (cursor_at, cursor_rank, cursor_id)
```

For a source with fixed rank `r`, the equivalent predicate is:

```sql
timestamp > :cursor_at
OR (timestamp = :cursor_at AND
    (r > :cursor_rank OR (r = :cursor_rank AND id > :cursor_id)))
```

Use the source's actual timestamp column and constant rank. The four cases are:

| Queried source | Cursor on log (rank 0) | Cursor on HTTP (rank 1) |
| --- | --- | --- |
| Logs (rank 0) | Later timestamp, or equal timestamp and larger log ID | Strictly later timestamp only |
| HTTP (rank 1) | Later or equal timestamp, regardless of HTTP ID | Later timestamp, or equal timestamp and larger HTTP ID |

IDs from different sources are never compared to decide equal-time order. In
particular, an HTTP cursor has already passed every log at that timestamp; a log
cursor has not yet passed any HTTP row at that timestamp. Test all four cases
with source IDs both smaller and larger than the cursor ID.

Merge, return the first `limit`, and use
the extra row to compute `has_more`. The cursor refers to the last emitted event,
not the last fetched row. Do not paginate by a bare source row ID or use OFFSET.

Repeated calls with the same cursor and unchanged evidence must return identical
events. After any supported change, reject continuation rather than skipping,
duplicating, or replacing events. A first-page response represents evidence at
`snapshot_at`; a fresh invocation may contain additional evidence.

A daemon restart alone does not invalidate a cursor: the cursor has no process
epoch and remains valid if this attempt's evidence is unchanged. Startup capture
finalization or retention can change the selected attempt's revision and cause
409 on continuation. Tell the operator to rerun `otter trace <run-id>` without
`--after`, discarding previously collected pages if assembling a complete trace.
Crash recovery also marks previously running attempts failed and appends lifecycle
lines, but v1 cannot issue a pre-restart trace cursor for those running attempts.
They become traceable after recovery; do not describe that transition as an
invalidation of a previously valid terminal-only cursor.

## Implementation sequence

### 1. Timeline types and read path

- Add a focused `internal/timeline` package for event/context types, cursor
  validation, revision calculation, and read-only timeline assembly. Keep it
  independent of CLI and API transport packages.
- Add transaction-aware projections in `internal/runs` and `internal/inspection`
  as needed. Reuse capture state derivation; do not reproduce its policy logic.
- Add the next available migration with indexes on
  `run_logs(run_id, timestamp, id)` and
  `http_exchanges(run_id, occurred_at, id)`. Do not edit old migrations.
- Implement chronological bounds, bounded merge, and revision checks. Read only
  projected columns. Do not load entire run logs into memory to sort them.
- Test this layer before exposing it through the CLI.

### 2. Daemon, API, and client

- Wire a timeline reader into the daemon and add a backend method for fetching
  a page. Check run existence and `Status.Terminal()` in the same read snapshot.
  Do not require capture to be complete: old, disabled, pending, or abrupt
  recordings must still allow inspection of a terminal attempt.
- Register the admin route and implement query parsing and error mappings.
- Add a typed API client method and update fake backends and fixtures.
- Avoid loading retry descendants solely for the timeline header. Show the
  selected attempt's parent relationship and `run-status` command instead, so
  an independently progressing retry does not churn this attempt's revision.

### 3. CLI and rendering

- Add `internal/cli/trace.go`, command dispatch, usage text, and the agreed flags.
- Reuse structured log formatting, duration formatting, and terminal escaping.
  Apply escaping after formatting log text so parsed fields cannot inject controls.
- Render context, capture state, events, and exact next-page instructions.
  Continuation instructions preserve `--no-http` and the chosen output mode.
- Use `otter request <run-id> <request-id>` for unambiguous payload inspection.
- Reuse capture explanations where accurate. Add trace-specific wording for
  expiry (exchange records and payloads removed; summary retained) and terminal
  attempts whose capture remains pending (recording not finalized, not “run is
  still executing”). Preserve existing commands' output in this milestone.
- Handle output write errors and produce typed JSONL context/page records even
  when the event list is empty.

### 4. Verification through the real execution path

Extend the generic daemon/CLI fixture approach in
`internal/daemon/capture_e2e_test.go` and `internal/cli/capture_test.go`. Use
temporary integrations and a local HTTP server, not vendor accounts.

Required scenarios:

1. An uninstrumented integration POSTs JSON, reads an HTTP 400 error body, and
   raises. Release and execute it normally. Trace shows context, the captured
   request/call site, exception output, and recorded terminal lifecycle. Its
   printed detail command retrieves the sanitized outgoing and response JSON.
2. A request blocks and the run times out. The trace retains the known request
   summary and reports incomplete capture without inventing a response or duration.
3. Execution fails before any HTTP call, including a pre-launch configuration
   failure. The trace explains the run failure despite absent HTTP evidence.
4. A retry chain: each finished attempt has its own trace and correct parent;
   a queued/running retry is rejected without suppressing its parent's trace.
5. A successful run with a handled HTTP error: display the HTTP status without
   claiming that the run failed.

Unit and integration checks:

- Interleaving, equal timestamps within/across sources, page sizes 1 and maximum,
  empty sources, exact-full final page, and one source much larger than another.
- Repeated cursor use, limit changes, invalid/oversized/unsupported cursors,
  wrong run/filter, and no gaps/duplicates across unchanged pages.
- All four source/cursor rank combinations at equal timestamps, including HTTP
  IDs smaller than a log cursor and log IDs larger than an HTTP cursor.
- Distinct running, retrying-with-parent, retrying-without-parent, and queued
  first-attempt hints. Unsupported `--follow` remains a usage error, not a
  nonterminal-run error.
- Retry creation leaves the new attempt's logs empty and writes the scheduling
  message only to its parent; manual/cron/webhook submission records its own
  queued lifecycle line.
- Late older-timestamped HTTP insert, update to an already displayed exchange,
  late terminal log, and retention between pages all invalidate continuation.
- Restart between pages: an unchanged terminal attempt's cursor remains valid;
  startup finalization of its pending capture or retention of its evidence
  invalidates continuation with 409 and restart-trace guidance. Separately,
  recover a previously running attempt and verify its first post-recovery trace
  includes the recorded recovery lifecycle and failed status; pre-restart trace
  access must have been rejected while it was running.
- Page reads see one database snapshot; no mixed pre/post-expiry response and no
  deadlock with the single database connection.
- Off, unavailable, pending, expired, incomplete, dropped events, no observed
  HTTP, no retained logs, and `--no-http` each explain missing evidence correctly.
- `ingested_at < occurred_at`, streamed requests, missing starts, and overlapping
  requests do not imply false causal or response ordering.
- API authorization, unknown/nonterminal runs, strict query validation, and
  absence of payload/header/trigger columns from the output projection.
- CLI JSONL parsing and framing, pretty/JSON precedence, flags after the run ID,
  exit codes, hostile terminal strings, shell-safe detail commands, and write errors.
- Existing `logs`, `requests`, and `request` behavior remains unchanged.

Inspect query plans for chronological page queries using both indexes. Apply a
250 ms internal context deadline to the entire database read operation, starting
before connection acquisition and including revision work and page queries (or
use the caller's earlier deadline). On expiration, close rows and roll back
promptly; return a controlled 503 inspection-timeout response and no partial page.
Do not automatically retry while retaining or reacquiring the connection.

The 10-second SQLite busy timeout is a failure mode, not the trace budget. Pool
waiting behind the single connection is separate from SQLite lock contention;
the internal deadline must cover both. Verify actual driver cancellation and
connection release under pool contention, a long query, and an external SQLite
write lock. A context deadline alone is not evidence that a blocked driver call
released the connection. Require release within 300 ms of operation start,
including cancellation/rollback, in the controlled cancellation tests. If this
cannot be met, change the read strategy or connection configuration before
shipping rather than relying on the 10-second timeout.

Performance gate: exercise at least 100,000 log rows and 1,000 exchanges, then
stress with 1,000,000 log rows, including first and continuation pages at limits
1, 100, and 1,000. Run concurrent log appends, HTTP ingestion, and queue writes;
compare against the same workload without trace traffic. Record hardware, page
latency, connection-hold time, memory, timeout rate, and write-latency deltas.
Target p95 additional write latency below 100 ms and no trace-induced write stall
over 300 ms on the documented reference machine, including repeated trace
requests. Successful page reads must finish inside the 250 ms budget; slow reads
must fail cleanly within the cancellation bound. Sustained failures at the
100,000-row fixture or breached write-stall targets block release and require a
cheaper revision/read strategy or admission control. Do not weaken consistency
to pass the gate. Avoid flaky wall-clock assertions in ordinary unit tests;
use controlled cancellation tests and a reproducible performance harness.

Run focused package tests during implementation, then the repository's required
checks (`make test`, `make lint`, and the applicable smoke workflow). No new Python
instrumentation tests are needed unless implementation actually changes the SDK.

### 5. Documentation and completion

- README: add trace to the CLI list and show the failed-run-to-request workflow.
- `docs/http-capture.md`: explain the merged view, capture degradation, HTTP
  summary placement, and actual expiry behavior.
- `docs/api-reference.md`: document route, page schema, cursor invalidation,
  terminal-only restriction, authorization, and status codes. Explain that
  startup evidence changes can invalidate continuation after a daemon restart,
  while restart alone does not; show how to restart the trace without `--after`.
- `docs/architecture.md`: explain the read-only merge and index migration.
- Align `docs/run-timeline-feature.md` with the shipped v1 scope during
  implementation, separating live following and trigger payloads into future work.
- Include examples of both human output and typed JSONL, including empty output
  and continuation. State explicitly that state mutations are not captured.

The milestone is complete when an operator can identify the failing operation
and reach its payload evidence from a trace without manually correlating commands,
and the timeout/no-HTTP cases remain equally understandable. All pages must either
describe consistent retained evidence or explicitly require a restart.

## Follow-up design: live following

Treat live following as its own design after validating the completed-run view.
It needs an explicit delivery/update contract: whether to emit request-start and
completion events, how consumers recognize revisions, and how late arrivals are
represented. Evaluate an append-only change sequence or a revision-aware stream;
an occurrence-time cursor alone cannot supply these guarantees. Do not add a
fixed sleep and call it gap-free chronology.
