# Run timeline: feature description

> **Status: broader proposal — v1 has shipped with a narrower scope.**
>
> This document is the original, wider proposal for `otter trace`. The first
> release is specified in
> [`docs/run-timeline-implementation-plan.md`](run-timeline-implementation-plan.md);
> the behaviour that actually shipped is described in
> [`docs/http-capture.md`](http-capture.md#the-merged-view-otter-trace) and in
> [`docs/api-reference.md`](api-reference.md). Where this document and those
> disagree, the shipped behaviour wins. The wider material is kept here — and
> marked section by section — as the roadmap for later work, with the deferred
> items collected in [Deferred to later work](#deferred-to-later-work).
>
> **Shipped v1 scope**
>
> - Command: `otter trace <run-id>` with `--limit`, `--after`, `--json`,
>   `--pretty`, `--no-http`.
> - One FINISHED attempt per trace (succeeded, failed, timed out, cancelled).
>   Queued, running and retrying attempts are refused with an actionable hint.
> - A context header plus merged lifecycle, stdout/stderr and HTTP summary
>   events, in `(at, source_rank, id)` order.
> - Typed JSONL: a `context` record, then `event` records, then a `page` record.
> - Continuations are bound to an evidence revision; a changed trace is refused
>   with `409` and the operator is told to restart.
> - An index-only migration for chronological pagination; no new table and no
>   SDK change.

## Summary

Add `otter trace <run-id>`: one chronological view of everything a run did. It
merges the run's lifecycle events, its `ctx.log` output, and its captured HTTP
exchanges onto a single timeline, so a failing run can be read top to bottom
instead of being reconstructed by holding three separate command outputs side by
side.

This is a read-only, presentation-layer feature. It adds no SDK instrumentation
and no new capture. The shipped v1 adds one index-only migration for
chronological pagination; it adds no new table.

## The problem it solves

Otter already records enough to explain a failure, but the record is split
across independent streams that share no common view:

| Recorded | Stored in | Read with today |
| --- | --- | --- |
| Integration stdout/stderr and daemon lifecycle lines | `run_logs` (streams `stdout`, `stderr`, `otter`) | `otter logs <run-id>` |
| HTTP exchanges, each with `occurred_at`, duration, call site and payloads | `http_exchanges` | `otter requests <run-id>`, `otter request <id>` |
| Trigger body/headers, release digest, attempt and retry chain | `runs` row and `runs.metadata` | `otter run-status <run-id>` |

The operator is the join. Diagnosing the canonical case — a JSON POST that
returns HTTP 400 and fails the run — means reading `otter logs` to learn *when*
it broke, switching to `otter requests` to learn *what* it sent, and mentally
aligning two independent id spaces and two timestamp columns. That is exactly
the work the HTTP capture milestone set out to remove, one level up.

`otter trace` closes it: the failure, the exchange that preceded it, and the log
line the integration wrote about it appear together in the order they were
recorded, with the request line pointing at the exchange detail that holds the
payloads. The trace reports that order; it does not label a request as the cause
of a failure.

## Product contract

Shipped in v1:

```sh
otter trace <run-id>                 # merged timeline, oldest first
otter trace <run-id> --after <cursor>  # resume from a previous cursor
otter trace <run-id> --limit N       # bound one page
otter trace <run-id> --json          # JSONL, one record per event
otter trace <run-id> --pretty        # force the human form when piped
otter trace <run-id> --no-http       # logs and lifecycle only
```

The original proposal also carried live following:

```sh
otter trace <run-id> --follow        # deferred: keep printing as the run progresses
```

`--follow` is **not shipped**, and it is not merely unimplemented: a trace is
only well defined for a finished attempt, because while a run is executing its
events are still arriving and there is no stable order to page over. v1 rejects
`--follow` as a usage error and points at `otter logs <run-id> --follow` instead;
it returns when live following does (see
[Deferred to later work](#deferred-to-later-work)). For the same reason a
`queued`, `running` or `retrying` attempt is refused with the next command to
run, rather than answered with a partial timeline.

Conventions follow `otter logs` and `otter requests` exactly:

- JSONL when `--json` is set or stdout is not a terminal; the human form on a
  terminal; `--pretty` overrides the pipe detection.
- `--after` takes the opaque cursor printed by a previous call, not a raw id.
- Terminal control characters in remote-influenced strings are escaped with the
  same helper the inspection commands use.
- Exit codes match the rest of the CLI: `0` success, `1` runtime/API failure,
  `2` usage.

A trace is never empty-but-silent. A run with no HTTP capture still renders its
logs; a run with neither renders the capture state and says so, using the same
vocabulary as `otter requests` (`off`, `unavailable`, `expired`, `pending`,
`incomplete`).

## Event model

A shipped timeline record is a union of three kinds. Every record carries the
fields the merge and the renderer need, and nothing else:

```text
kind        lifecycle | log | http
at          the ordering timestamp
source      run_logs | http_exchanges
id          the source row id, unique within source
run_id      the owning run
```

The page also carries a context header, which is framing rather than an event:
run identity and name, status, error and exit code, attempt and parent run id,
release digest, capture state, and the trigger **type** only.

Kind-specific payloads:

- **lifecycle** — the daemon and SDK's own narration: `run queued`, `run
  started`, `run cancelled`, `marked failed`, terminal status. These are the
  existing `otter`-stream log lines; the kind is derived from the stream, not
  stored separately.
- **log** — an integration `stdout`/`stderr`/`ctx.log` line: stream, message,
  and structured fields. Fields are not a column: the daemon appends them to the
  stored message as trailing JSON, and the CLI recovers them exactly as
  `splitStructured` already does for `otter logs`. The trace reuses that parsing
  rather than introducing a second interpretation of the same line.
- **http** — method, sanitized URL, status or transport-error class, duration,
  phase, payload completeness, `request_id`, and call site. Never a payload:
  bodies stay behind `otter request <request-id>`, which the human form prints
  as the follow-up command.

The proposal's fourth kind, **trigger**, is deferred and is not emitted in v1. It
was to render the sanitized trigger body and headers from `runs.metadata` as the
first record, so the input to the run would be visible without a second command.
Trigger payloads are stored **unsanitized** — unlike captured HTTP, they never
passed through the capture redaction contract — so emitting them would disclose
a header or body that no existing inspection command shows. v1 therefore puts
only the trigger type in the context header and never writes a trigger event.
Displaying the trigger needs a bounded redaction contract first; see
[Deferred to later work](#deferred-to-later-work).

## Ordering and the cursor

Records are merged from per-source queries, oldest first.

Ordering key is `(at, source_rank, id)` with a stable source rank so that two
records with the same timestamp order deterministically. The cursor is that
whole tuple, opaque to the caller and encoded server-side; it is not a raw row
id, because `run_logs.id` and `http_exchanges.id` are independent sequences and
a bare id would silently skip or replay events.

Each source is queried with its own `after` bound derived from the tuple, one
record beyond the requested limit per source, so the merge can decide the next
page without reading a whole source. `--limit` bounds the merged page, not each
source.

### Page consistency

A page is not a database snapshot: the sources can still move between pages — a
late lifecycle line, an exchange updated by in-flight capture delivery, retention
removing exchanges. The shipped v1 keeps the continuation stateless but binds it
to the evidence it started from. The opaque cursor carries a revision digest of
the run context, the capture state and each selected source's `(MIN(id),
MAX(id))` bounds. Those bounds are two covering index seeks rather than a count
over the run's rows, so revisioning a page stays O(1) instead of growing with the
run. On a continuation the server recomputes the revision and compares it with
the cursor's:

- equal — the page is served;
- changed — the request is refused with `409` and the operator is told to start
  the trace over, rather than being handed a page that silently skips, repeats or
  replaces events.

A daemon restart on its own does not change the evidence and so does not
invalidate a cursor.

### Clock honesty

Ordering is approximate chronology, not causality. `http_exchanges.occurred_at`
is stamped by the integration process; `run_logs.timestamp` is stamped by the
daemon. On the supported topology the child runs locally under the daemon, so
the two usually share a host clock — but sharing a clock does not mean the events
are *observed* in that order. Capture delivery is buffered and batched, so an
exchange can be ingested after log lines the daemon stamped later than it, and
the merged order then reflects observation, not cause.

The feature must not paper over this:

- The wire record carries both `at` (the ordering timestamp) and `ingested_at`
  for HTTP records, so a consumer that distrusts producer clocks can re-sort.
- When an HTTP record's `ingested_at` is earlier than its `occurred_at`, the
  human form marks the line rather than presenting a possibly-wrong order as
  fact.
- Documentation states that ordering is by the recording process's clock, is
  approximate chronology rather than causality, and is not guaranteed to hold
  across hosts if the producer and daemon topology changes.

## API

```text
GET /v1/runs/{id}/timeline?after=<cursor>&limit=N&include_http=true
```

- Operator-authorized, matching `GET /v1/runs/{id}/requests`. It exposes
  sanitized metadata already reachable through `otter logs`, `otter requests`
  and `GET /v1/runs/{id}`, so it introduces no new disclosure, and it must not
  widen any: payloads are excluded exactly as in the request list.
- Response is a page of records plus the next cursor and a capture state block
  identical in shape to the one `otter requests` already prints.
- Unknown run: `404`, with the same error envelope as the rest of the API.
- A run that has not finished: `409`, naming the status and the next command to
  run instead.
- Malformed, oversized, unknown-version or wrong-run cursor: `400`, with a
  message saying to start the trace over rather than silently restarting it. A
  cursor that is well formed but was minted against different evidence: `409`,
  with the same instruction.
- `include_http=false` supports `--no-http` server-side; the merge then reads
  only `run_logs`.

No new ingestion endpoint. Nothing in the SDK changes. The shipped v1 adds one
index-only migration (`0007_timeline_indexes.sql`) so each page is a bounded
ordered seek instead of a full sort of the run's rows; it adds no new table, and
the merge still reads the existing tables.

## Coverage and degradation

The feature inherits capture's honesty rules rather than inventing new ones:

- Capture `off` or `unavailable` does not suppress the trace. The timeline shows
  logs and lifecycle and states the capture situation in one line, because "no
  HTTP shown" and "HTTP was not recorded" are different answers.
- Capture `incomplete` or with dropped events is surfaced on the trace, so a
  missing request is never read as a request that never happened.
- `expired` says retention removed the recorded exchanges, so no HTTP lines
  appear; only the run's capture summary survives. The proposal's "keep the
  exchange metadata lines after payloads expire" is deferred (see
  [Deferred to later work](#deferred-to-later-work)).
- A trace of a `urllib`/`requests`/`httpx` run states adapter coverage exactly as
  `otter requests` does. Other clients and raw sockets remain uncaptured, and
  the trace must not imply otherwise.

## What it is not

- **Not replay.** The timeline reads; it does not re-issue requests.
- **Not state history.** State reads and writes are not on the timeline in this
  version. Adding them needs a state-change event source that does not exist
  yet, and is a separate feature.
- **Not a debugger or an interactive UI.** Output is line-oriented and
  pipe-friendly.
- **Not a new store.** No timeline table, no denormalized copy, no background
  indexer. The shipped v1 adds two indexes so pagination is a bounded seek; an
  index is not a store. If a future version needs a table, that is a deliberate
  change with its own migration.

## Deferred to later work

v1 ships the join above for one finished attempt. These parts of the proposal are
not in it:

- **Live following and live updates.** `--follow` cannot be honoured while events
  are still arriving, and a run's order is not stable until it finishes.
- **Trigger body and header display.** Stored trigger payloads are unsanitized,
  so showing them needs a bounded redaction contract first. v1 shows the trigger
  type in the context header and emits no trigger event.
- **State-change events on the timeline.** `ctx.state.get`/`set`/`delete` are not
  recorded as events, so there is no state-change source to merge yet.
- **Retaining HTTP metadata after expiry.** Retention deletes exchange rows and
  keeps only the run capture summary, so an expired run shows no HTTP lines.
- **Replay.** The timeline stays read-only; re-issuing requests is a separate
  feature.

## Acceptance scenario

The existing HTTP-milestone fixture: an integration POSTs JSON, receives HTTP
400, reads the error body, and fails.

1. `otter run .` fails and prints a run id.
2. `otter trace <run-id>` shows, in order, `run queued`, `run started`, the
   integration's log line, the `POST ... 400` exchange line with its call site,
   the error line, and the terminal `run failed` lifecycle line. The trigger type
   appears in the context header; no trigger payload is shown.
3. The exchange line names the `otter request <request-id>` command that prints
   the sanitized outgoing JSON and the error response.
4. The same run traced with `--json` yields a typed stream — one `context`
   record, one `event` record per event, one `page` record — and `--after`
   continues from the printed cursor to the next page without duplication or
   gaps. There is no live follow in v1.

The v1 feature is complete when that scenario works through the real CLI, daemon
and database, with no manual log statements in the integration and no manual
correlation by the operator.

## Verification

1. Merge unit tests: interleaving of lifecycle, log and HTTP records; equal
   timestamps; empty sources; one source far ahead of another; cursor round-trip
   with no duplicates or gaps across pages; a continuation whose evidence
   revision changed is refused rather than answered with a hole.
2. Ordering tests: stable ordering under equal keys; `ingested_at` earlier than
   `occurred_at` is marked, not silently reordered.
3. Degradation tests: capture `off`, `unavailable`, `expired`, `incomplete` and
   dropped-events states each render logs and the correct explanation.
4. Authorization: the endpoint rejects a run token where an operator token is
   required, matching the request-list behaviour; cross-run access fails.
5. CLI tests: JSONL shape and record types, `--pretty` over a pipe, `--limit`
   paging, `--after` resume, `--no-http`, `--follow` rejected as usage,
   a non-terminal attempt refused with the next command, terminal escaping of a
   hostile log line, and the usage/exit codes.
6. End-to-end: the acceptance scenario through the normal release/run path, plus
   a retry chain, asserting each finished attempt gets its own timeline and the
   parent relationship is visible.
7. Regression: `otter logs`, `otter requests` and `otter request` output is
   unchanged; trace adds no measurable latency to run execution (it is
   read-only) and no new writes to `run_logs` or `http_exchanges`.

## Documentation

The shipped v1 documents `otter trace` in the README CLI block and in
[`docs/http-capture.md`](http-capture.md#the-merged-view-otter-trace), with the
endpoint in [`docs/api-reference.md`](api-reference.md). Both state that state
changes are not on the timeline and that ordering is approximate chronology, not
causality. An event-kind table in `docs/architecture.md` under logging remains
future work, as does documenting live following once it exists.
