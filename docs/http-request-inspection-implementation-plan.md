# HTTP request inspection: implementation handoff

## Objective and scope

Implement the first debugging milestone: a developer can inspect a run's outgoing
HTTP requests and responses without adding logging statements to integration code.

The acceptance scenario is a JSON POST returning HTTP 400. After the integration
fails, `otter requests <run-id>` lists the request and
`otter request <run-id> <request-id>` shows its sanitized outgoing JSON and error
response. Existing execution, exception, retry, and response-reading behavior is
preserved.

Implement this milestone end to end. Do not implement state history, replay,
local-source execution, debugger attachment, a combined timeline, a browser UI,
or adapters for requests/httpx in this change. Do not restore vendor integrations
or connector libraries removed from this repository.

## Repository facts and initial checks

This plan was checked against the working tree on 2026-09-22. Recheck applicable
AGENTS.md instructions and current files before implementation; preserve unrelated
changes. Relevant existing components:

- `sdk/python/otter/_launcher.py`: runs the module and deferred decorated entrypoint.
- `sdk/python/otter/runner.py`: reports failures and uses `os._exit`; shutdown-only
  capture flushing will miss failures.
- `sdk/python/otter/_client.py`: urllib-based runtime API client with retries.
- `internal/executor/executor.go`: launches the SDK module and builds child env.
- `internal/daemon/view.go`: run submission and trigger metadata.
- `internal/daemon/workers.go`: execution, token lifetime, retries.
- `internal/api/{server,backend,client,types}.go`: routes, authorization and contracts.
- `internal/runs/`: persisted run records and logs.
- `internal/cli/cli.go`: command dispatch and run flags.
- `internal/database/database.go`: SQLite uses one connection. Queries inside a
  transaction must use that transaction, not the outer DB connection.
- `migrations/`: currently ends at `0003_integration_identity.sql`; add the next
  migration, never modify an applied migration.
- `internal/daemon/identity_api.go`: reset/delete paths that must clean up capture data.

The previously discussed `lib/python/otter_connectors/http.py` no longer exists.
Use SDK-owned, process-local urllib instrumentation installed by the launcher.
Keep fixtures generic and package-local or generated in temporary directories.

## Product contract

Proposed CLI:

```sh
otter run . --capture full
otter requests <run-id>
otter request <run-id> <request-id>
otter requests <run-id> --json
otter request <run-id> <request-id> --json
```

Support `off`, `metadata`, and `full`. New normal runs default to `metadata`;
full payload capture is an explicit per-run option. Historical runs without
capture configuration are `unavailable`, not recordings containing zero calls.
Retries inherit the submitted policy, but each attempt owns its own requests.
Cron and webhook runs use metadata for this milestone. Do not interpret webhook
input as capture configuration.

Metadata includes method, sanitized URL, status, duration, transport-error class,
and call site. Full adds permitted headers and bounded sanitized JSON bodies.
Unknown/text/binary/form bodies show metadata and an explicit unsupported-content
reason in v1. Document this limitation instead of promising universal payload
inspection. Header and URL redaction applies even in metadata mode.

Capture observes live requests; it does not change their destinations or suppress
writes. Normal runs still execute immutable releases. Preserve the current run
command's wait/no-wait behavior and output conventions.

## 1. Capture policy and wire contract

Add a small versioned inspection contract, independent of trigger payloads and
logs. Define Go and Python representations and table-driven conformance fixtures
for redaction and completeness states.

For manual submission, use a validated query option on the existing endpoint,
for example `POST /v1/integrations/{id}/runs?capture=full`. This preserves the
existing request body as the trigger JSON verbatim. Extend the internal submission
options and client methods cleanly; do not hide the option inside user JSON or
record authentication headers as configuration.

Persist the resolved policy with the run and pass only server-resolved settings
to the launcher. Record policy/schema version and adapter coverage (`urllib`).
Capture disabled means no instrumentation is installed.

Initial centrally defined limits:

- 256 KiB buffered per request or response body.
- 10 MiB persisted capture data per run, including serialized metadata.
- 1,000 request records per run; bound header count/size, URL length, stack depth
  and ingestion batch size as well.
- Seven days of capture retention, configurable on the daemon.
- A bounded SDK queue and a bounded total shutdown flush deadline (two seconds).

Enforce limits in both SDK and server. Keep a small run summary after retention
so expired capture is distinguishable from no observed requests. The summary
includes known dropped counts and finalization status; abrupt termination means
loss is possible, not that an exact count is known.

## 2. Persistence and API

Create `internal/inspection` for capture types, validation, redaction and storage.
Use SQLite, not a new service or generic tracing platform.

Store a per-run capture summary and HTTP exchange records. A record needs:

- Run ID plus SDK-generated request ID; a unique constraint on that pair.
- Producer sequence and occurrence time; stable database cursor for pagination.
- Method, sanitized URL, call site (file/function/line; no local variables).
- Start, response-header, completion timing and explicit in-progress status.
- Status code or sanitized transport error; initial and final URL for redirects.
- Ordered header pairs, preserving duplicate names where supported.
- Separate request/response body descriptors: content type, bytes observed,
  captured JSON, and omitted/redacted/partial/over-limit states.

Use start, response and completion updates keyed by request ID. Make duplicate
delivery idempotent and reject stale updates that regress a finalized exchange.
One exchange represents one `OpenerDirector.open` call; redirects internal to
urllib are represented by initial/final URL, not falsely presented as separately
captured hops. Application retries produce separate records.

Endpoints:

```text
POST /v1/runs/{id}/requests/events
GET  /v1/runs/{id}/requests?after_id=&limit=
GET  /v1/runs/{id}/requests/{request_id}
```

The POST accepts bounded batches of allowlisted event types. Require a live run
token scoped to the path's run, infer integration identity from the run, and
reject forged/cross-run attribution. Use operator authorization for GETs, matching
existing loopback/admin rules. Never expose raw bodies through error responses.
List responses exclude payloads and include capture coverage/completeness.

Enforce body/row quotas transactionally. A quota rejection is diagnostic loss,
not an integration failure. Cascade capture deletion with run deletion and cover
integration reset/delete. Add periodic bounded retention work and startup cleanup;
avoid long cleanup transactions blocking the single DB connection.

## 3. SDK transport and urllib instrumentation

Add private SDK modules for capture lifecycle/transport and urllib adaptation.
Keep the SDK standard-library-only.

Install instrumentation before `runpy.run_path`, so imports and module-level
requests are covered. Wrap `urllib.request.OpenerDirector.open` once per process,
which covers normal `urlopen` and custom openers using the base implementation.
Account for nested calls during redirects to avoid duplicates. Preserve the
original callable and do not alter global behavior when capture is disabled.

Explicitly exclude Otter control API calls. Use a thread-local suppression guard
for capture delivery and SDK client requests, including overridden API paths.
Do not recursively record ingestion, state, trigger or log requests.

Record a start before calling the original transport. Return a delegating response
wrapper which observes bytes as the application consumes them. Preserve read,
readline, iteration, readinto where provided, headers, status, geturl, context
manager and close behavior. Do not pre-read responses, rewind request streams,
or consume iterable request bodies. Unsupported request streams are omitted.
Do not fabricate a complete body when the application reads only a prefix.

HTTPError requires special care: keep it catchable as HTTPError and preserve its
readable body, headers and status. Capture the error stream as callers consume
it; do not consume it while constructing the event. Preserve transport exception
types and propagation. Test these behaviors rather than replacing errors with
generic wrappers.

V1 captures bounded, valid UTF-8 JSON, including `application/*+json`, after full
consumption. Omit oversized, incomplete, encoded or unparseable bodies with a
specific reason. Do not retain an unsafe truncated JSON prefix. Empty bodies are
different from omitted bodies. Track duration to headers separately from duration
through body close/EOF.

Use a bounded queue and a background delivery worker. Do not reuse the SDK
client's normal long retry policy unchanged: capture has short timeouts, bounded
retries, and idempotent delivery. Queue pressure must drop capture rather than
block application network calls. Keep any buffered raw bytes bounded and in
memory only, and release them promptly after sanitization or omission.

Flush from both launcher cleanup and the runner's failure exit path before
`os._exit`. Keep the token valid until the child exits as today. Mark undelivered
or unfinished capture incomplete; SIGKILL cannot guarantee a final flush. Avoid
catching application BaseException merely to conceal instrumentation failures.

Coverage must say `urllib`, not `all HTTP`. Custom overrides that bypass the base
opener, subprocesses, requests, httpx and raw sockets are outside v1 coverage.

## 4. Redaction and failure behavior

Apply policy before serialization/delivery and defensively again in Go before
storage. Use common fixtures so both implementations agree.

- Remove URL userinfo and fragments; redact sensitive query values while
  preserving repeated parameters and non-sensitive values.
- Redact Authorization, Proxy-Authorization, Cookie, Set-Cookie, common API-key
  headers and configured header names, case-insensitively.
- Recursively redact common credential field names (password, secret, token,
  access_token, refresh_token, api_key, client_secret) in JSON objects/arrays.
- Allow additive operator-configured query/header/JSON-field rules; callers
  cannot weaken mandatory defaults through event submissions.
- Bound and sanitize exception text; never blindly persist str(exception),
  which can contain a URL or credentials.

If content cannot be safely processed under the supported policy, omit it.
Document that field-based redaction does not discover every secret or personal
value in arbitrary data. Full capture remains opt-in.

Capture failures must preserve the integration's return values, exceptions and
exit status. Surface loss through the capture summary when delivery is possible,
and at most one SDK diagnostic per run when it is not. Never print failed payloads
or tokens in that diagnostic.

## 5. CLI and documentation

Implement commands in focused new CLI files rather than growing `cli.go` beyond
dispatch/help changes. Follow current flag normalization, error and JSON styles.
Add `capture` to run's value-taking flag handling.

`requests` lists request ID, method, sanitized URL, status/error, duration and
completeness. Support `--limit` and `--after-id`. Do not load every body to render
the list. `request` prints metadata, sanitized headers and pretty-printed JSON
request/response bodies. Escape terminal control characters in remote strings.
Both commands have stable JSON output. Cross-run request lookup returns not found.

Explicit messages distinguish off, unavailable, enabled/no observed calls,
incomplete capture, unsupported bodies and expired capture. Do not imply that
zero captured calls proves no network activity occurred.

Update README, SDK README, API reference, operations, and security documentation
with supported coverage, full-capture usage, limits, retention, redaction and live
network behavior. Do not document unimplemented future adapters as available.

## 6. Required tests and verification

Use local HTTP fixtures only; no live SaaS credentials or calls.

1. Migration on a fresh DB and an existing DB; historical runs remain readable.
2. Store lifecycle, duplicate delivery, out-of-order updates, pagination, quotas,
   retention and cleanup on run/integration deletion.
3. API token scope, revoked-token rejection, invalid events, oversized batches,
   and rejection of cross-run request lookup/ingestion.
4. Redaction fixtures shared across Go/Python: nested arrays, mixed-case headers,
   repeated query parameters, userinfo, malformed JSON, partial/oversized bodies,
   exception URLs and control characters. Inspect persisted rows for sentinels.
5. urllib GET, JSON POST, HTTP 400 readable body, timeout, refused connection,
   redirect, custom opener, partial read, iteration, readinto, context manager,
   close without read, streamed upload omission and two concurrent requests.
6. Compare client-visible results/errors with capture off/on. Verify no recursive
   Otter traffic and no modifications outside an Otter-launched process.
7. Queue saturation and ingestion outage: integration still finishes; memory and
   shutdown are bounded. Failure exit flushes; killed runs show uncertainty.
8. Generic end-to-end integration: GET JSON, POST transformed JSON, receive 400,
   read error body, fail. Execute through normal release/run machinery. Assert
   request list and detail output reveal payload/error without explicit logging.
9. Retry inheritance and per-attempt separation; trigger JSON remains byte/meaning
   compatible with existing submission behavior; default run waiting is preserved.

Run focused Go/Python suites during development, then the repository's documented
test/lint commands from CONTRIBUTING.md and Makefile. At minimum include
`go test ./...` and `python3 -m unittest discover -s sdk/python/tests`. Report any
environmental failures accurately. Benchmark off/metadata/full against a local
fixture and report latency, process memory and DB growth; investigate unbounded
growth or network-call blocking before declaring completion.

## Implementation order and completion report

Deliver these dependency-ordered slices; continue through all of them:

1. Policy/types, migration and persistence tests.
2. Ingestion/read APIs, authorization and retention.
3. SDK transport, redaction, instrumentation and lifecycle flushing.
4. Run option plumbing and inspection commands.
5. End-to-end coverage, regression checks and documentation.

Use existing conventions for routine choices; do not expand scope to build a
generic event framework. No dependency on state history or debugger work is
necessary. Do not claim replay-ready recordings: v1 omits unsupported data and
does not capture all nondeterministic inputs.

Final report: describe implemented behavior, commands to try, test results,
measured overhead and known capture limitations. The milestone is complete only
when the failed-POST acceptance scenario works through the actual CLI, SDK,
daemon and database, with no manual logging in the integration.
