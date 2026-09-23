# HTTP request inspection

Otter records a run's outgoing HTTP exchanges so a failing integration can be
diagnosed without adding logging statements to integration code. `otter requests`
lists what a run sent, and `otter request` shows one exchange with its sanitized
headers and bodies.

Capture is configured per run. A normal run records request summaries; full
payload capture is opt-in because bodies routinely carry credentials. Recording
is diagnostic: it observes live traffic and never changes it.

## Commands

```sh
otter run . --capture full              # or --capture metadata / --capture off
otter requests <run-id>                 # list; supports --limit and --after-id
otter request <request-id>              # detail; the owning run is resolved for you
otter request <run-id> <request-id>     # detail; use this to disambiguate
```

`--capture` accepts `off`, `metadata` or `full` and defaults to `metadata`.
Both inspection commands support `--json` (the global flag) and `--pretty`; when
stdout is not a terminal they emit JSON automatically, following the existing
`otter logs` convention. `--pretty` forces the human form even when the output is
piped.

`otter request` takes the request id on its own, so the run id is normally
unnecessary: the daemon reads the owning run from the stored exchange rather than
expecting the caller to name it. A request id is only unique within a run, so an
id that more than one run recorded is reported as a conflict naming those runs
rather than resolved to an arbitrary one — pass the run id in that case.

`otter requests <run-id>` prints the capture state before the list, because an
empty list has several distinct meanings — capture observed nothing, capture was
disabled, or the run was never recorded at all:

```
capture: complete
  requests: 2 (2 completed, 0 failed, 0 incomplete)
  redacted: 3 values were removed before storage
  coverage: urllib only; other clients and raw sockets are not captured
REQUEST ID                        METHOD  STATUS  DURATION  PAYLOADS  URL
1a0d061cd2124c1c9d5abad3e7b41157  GET     200     6ms       full      http://127.0.0.1:8791/cursor
87603b35e60c4dae9f57040b15e24ab3  POST    400     1ms       full      http://127.0.0.1:8791/records
```

`otter request <request-id>` prints one exchange, including the request
and response headers and the sanitized JSON bodies when the run used `full`:

```
POST http://127.0.0.1:8791/records
  request:   87603b35e60c4dae9f57040b15e24ab3
  phase:     completed
  status:    400
  duration:  1ms (headers 1ms, body 0s)
  occurred:  2026-09-22T21:42:17.501585Z
  call site: main.py:27 in main
  payloads:  full

request headers
  Content-type: application/json
  Authorization: REDACTED

request body
  {
    "cursor": "cur-42",
    "items": [ 1, 2, 3 ]
  }

response headers
  Content-Type: application/json
  Content-Length: 67

response body
  {
    "error": "cursor rejected",
    "access_token": "REDACTED"
  }
  (1 value(s) redacted before storage)
```

The list never loads bodies, so it stays cheap even for a run with many
exchanges. `--limit` bounds the page (default 100) and `--after-id` continues
from the last `id` of the previous page; when a page is full, the command prints
the cursor to use next. The same data is available over HTTP — see the
[API reference](api-reference.md#http-request-inspection).

## Capture levels

| Level | Records | When it applies |
| --- | --- | --- |
| `off` | Nothing. No instrumentation is installed in the child process. | Explicit choice. |
| `metadata` | Request summaries: method, sanitized URL, status, duration, transport-error class and call site. Never a header or a body. | The default for normal runs. |
| `full` | Everything `metadata` records, plus permitted headers and bounded, sanitized JSON bodies. | Opt-in per run. |

Cron and webhook runs use `metadata`. A retry inherits the policy its parent was
submitted with, and each attempt owns its own recording, so an attempt's requests
are never merged with its parent's.

## What is captured

- The method and the sanitized URL, plus the initial and final URL when a
  redirect was followed.
- Request and response headers (at `full`).
- The status code, including unsuccessful responses such as `4xx` and `5xx`.
- Transport errors, with a sanitized message and an exception class rather than a
  verbatim exception string.
- Time to response headers, body-consumption duration and total duration.
- The nearest call site as `file:line` in a function.

Start, response and completion are recorded as separate updates, so a request
whose process was killed mid-flight remains visible as in progress rather than
disappearing.

## Coverage and its limits

Coverage is the Python standard library `urllib` transport only. The SDK
launcher installs the instrumentation before integration code is imported, so
requests made at import time are covered. Coverage is always reported as
`urllib`, never as "all HTTP".

Not covered in this version:

- `requests` and `httpx`.
- Custom openers that bypass `urllib.request.OpenerDirector.open`.
- Subprocesses.
- Raw sockets.

Redirects followed internally by urllib appear as an initial and final URL on one
exchange, not as separate hops. Application-level retries appear as separate
exchanges. Otter's own daemon API traffic is excluded, so capture cannot recurse
into its own delivery.

## Bodies in v1

Only bounded, valid UTF-8 JSON is captured, including `application/*+json`, and
only after the application has consumed the body. Everything else is omitted with
a specific reason that `otter request` prints, for example:

```
  omitted: the application did not read the body to the end, so there is no complete value
```

A body that is text, binary, form-encoded, content-encoded (gzip/br), oversized,
unparseable, or only partially read is omitted. A truncated JSON prefix is never
stored. The request body is not read if it is a stream or an iterable, because
reading it would change what the request sends.

## Limits and retention

- 256 KiB buffered per request or response body.
- 10 MiB persisted capture data per run, including serialized metadata.
- 1,000 request records per run; header count and size, URL length, call-site
  length and ingestion batch size are also bounded.
- Seven days of capture retention by default, configurable with the daemon's
  `--capture-retention` flag. `--capture-retention 0` disables automatic expiry.
- Capture is delivered in the background with a bounded queue, short timeouts and
  bounded retries. Queue pressure drops capture rather than blocking an
  integration's network calls. Shutdown flushes within a two-second deadline.

Retention removes payloads but keeps a small per-run summary, so an expired
recording is never mistaken for one that observed nothing.

## Redaction

Redaction is applied in the SDK before delivery and again in the daemon before
storage. The mandatory rules cannot be weakened by a caller.

- URL userinfo and fragments are removed, and sensitive query values are
  redacted.
- `Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, common API-key
  headers and configured header names are redacted case-insensitively.
- Credential-shaped JSON field names — `password`, `secret`, `token`,
  `access_token`, `refresh_token`, `api_key`, `client_secret` and similar — are
  redacted recursively.
- Exception text is sanitized and bounded rather than stored verbatim.

Field-based redaction does not discover every secret or personal value in
arbitrary data. That is why `full` capture is opt-in, and why bodies are only
stored when they can be safely processed. Redaction and URL sanitizing apply in
`metadata` mode too, although `metadata` stores no headers or bodies.

## What capture does not do

- Capture observes live requests. It does not change their destinations, suppress
  writes, or make anything a dry run.
- An unavailable recording — a run that predates capture, or a run submitted with
  capture off before that was recorded — is reported as `unavailable`, never as a
  recording containing zero requests. A complete recording that observed nothing
  says so explicitly.
- Capture failures (queue pressure, delivery outage) never change an
  integration's return values, exceptions or exit status.
