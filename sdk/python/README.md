# Otter Python SDK

A thin, **standard-library-only** client for writing [Otter](../../README.md)
integrations in Python 3.8+.

The `otterd` daemon runs each integration as a child process
(`python3 main.py` inside the integration directory) and exposes a small local
HTTP API. The SDK wraps that API. It is deliberately thin: **the daemon is the
single source of truth** for state, run metadata and logs — nothing is cached
except the run metadata the SDK reads lazily for the trigger payload.

```
sdk/python/
├── otter/
│   ├── __init__.py     # public exports: Context, run, OtterError, ...
│   ├── _client.py      # internal HTTP client (urllib) + OtterError
│   ├── _capture.py     # bounded capture: redaction, queue, delivery
│   ├── _urllib_capture.py  # urllib adapter
│   ├── _requests_capture.py # requests adapter (when installed)
│   ├── _httpx_capture.py   # httpx adapter, sync and async (when installed)
│   ├── context.py      # Context.from_environment()
│   ├── state.py        # ctx.state
│   ├── log.py          # ctx.log
│   ├── trigger.py      # ctx.trigger
│   └── runner.py       # the @run decorator
├── tests/test_sdk.py   # unittest + fake in-process daemon
└── pyproject.toml
```

## The environment contract

The daemon sets these variables for every child process:

| Variable                 | Meaning                                              |
| ------------------------ | ---------------------------------------------------- |
| `OTTER_INTEGRATION_ID`   | Durable integration identity; namespaces state.       |
| `OTTER_INTEGRATION_NAME`  | Manifest label, e.g. `counter`; for logs and messages. |
| `OTTER_RUN_ID`           | UUID of the current run.                             |
| `OTTER_API_URL`          | Base URL of the daemon API, e.g. `http://127.0.0.1:7337`. |
| `OTTER_STATE_TOKEN`      | Per-run bearer token, scoped to this run/integration. |
| `OTTER_TRIGGER_TYPE`     | `manual`, `cron` or `webhook`.                       |
| `OTTER_INTEGRATION_DIR`  | Absolute path of the integration directory.          |
| `OTTER_CAPTURE_POLICY`   | `off`, `metadata` or `full`; unset means capture is off. |

The child's working directory is the integration directory. Ports are never
hardcoded — always read `OTTER_API_URL`.

## Quick start

```python
from otter import run

@run
def main(ctx):
    count = ctx.state.get("count", 0) + 1
    ctx.log.info("Counter executed", count=count)
    ctx.state.set("count", count)
```

`@run` executes the decorated function once the module has **finished loading**,
when `OTTER_RUN_ID` is present. Under the daemon, `python3 main.py` therefore
*is* the run; no `if __name__ == "__main__":` block is needed. When
`OTTER_RUN_ID` is absent (plain import, unit tests), the decorator is a no-op
and returns the function unchanged.

Running after the module body completes (rather than at decoration time) means
helpers, constants and classes may be defined anywhere in the file, including
below `main`, exactly as in an ordinary Python program.

Zero-argument functions are supported too. The context is passed only when the
function accepts a positional parameter:

```python
@run
def main():
    print("no context needed")
```

### Success and failure

* On success the process exits with code `0`.
* On failure the SDK logs the full traceback via `ctx.log.error(...)`, prints the
  same traceback to stderr, and exits with code `1` (`SystemExit(1)`) so the
  daemon records a failed run and applies its retry policy.
* Importing the same module twice in one process cannot execute a decorated
  function twice.

## Public API

```python
from otter import Context, run, OtterError
```

### `Context`

```python
ctx = Context.from_environment()   # reads the OTTER_* variables
ctx.run_id                         # str
ctx.integration_id                 # str
ctx.trigger                        # Trigger
ctx.state                          # State
ctx.log                            # Logger
ctx.api_url                        # str
ctx.integration_dir                # str (absolute integration directory)
```

`Context.from_environment()` raises `OtterError` when `OTTER_API_URL`,
`OTTER_RUN_ID` or `OTTER_INTEGRATION_ID` is missing.

### `ctx.state` — daemon-owned key/value state

```python
ctx.state.get(key, default=None)   # JSON value, or default on HTTP 404
ctx.state.set(key, value)          # persist any JSON-serializable value
ctx.state.delete(key)              # True if it existed, False if it did not
ctx.state.all()                    # {key: value} for the whole integration
```

* Keys must match `[A-Za-z0-9._:-]{1,128}`; other keys raise `OtterError`.
* Values are serialized with `json.dumps(..., allow_nan=False)`; a
  non-serializable value (or `NaN`/`Infinity`) raises `OtterError`.
* `set()` returns the value the daemon stored.
* HTTP 4xx/5xx responses raise `OtterError` with the daemon's error message.

### `ctx.log` — structured logs

```python
ctx.log.debug("...", **fields)
ctx.log.info("...", **fields)
ctx.log.warning("...", **fields)   # ctx.log.warn(...) is an alias
ctx.log.error("...", **fields)
```

Each call synchronously `POST`s to `/v1/runs/{run_id}/logs` with
`stream: "otter"`. The daemon's log endpoint accepts only
`stream`/`message`/`fields`, so the severity travels inside `fields` as
`"level"`. Fields are merged with (and cannot override) the level.

Logging never raises and never crashes an integration: if the request still
fails after the client's retries, the record is written to stderr as one JSON
line — `{"level": "info", "message": "...", "fields": {...}}`. The logger holds
a lock, so it is safe to call from worker threads.

### `ctx.trigger` — how this run started

```python
ctx.trigger.type        # "manual" | "cron" | "webhook" | "unknown"
ctx.trigger.body        # parsed webhook body, else None
ctx.trigger.headers     # {name: [values]} for webhooks, else {}
ctx.trigger.metadata    # the run's full metadata dict (possibly {})
ctx.trigger.header("Content-Type")   # first value or None (case-insensitive)
```

`type` prefers `OTTER_TRIGGER_TYPE` and falls back to the run metadata's `type`.
`body`, `headers` and `metadata` lazily `GET /v1/runs/{run_id}` on first access
and cache the result for the rest of the run.

### `OtterError`

Raised for genuine SDK/misuse failures: a missing `OTTER_API_URL`, an invalid
state key, a non-serializable state value, an API error response, or a transport
failure that survived every retry.

## Internal HTTP client

`otter._client.Client(api_url, token=None, timeout=10.0)` is the only place that
talks HTTP (via `urllib.request`). It offers `get_json`, `put_json`,
`post_json` and `delete_json`, each returning `(status_code, parsed_json_or_None)`:

* `Authorization: Bearer <token>` on every request when a token is set.
* `Content-Type: application/json` and a JSON body on writes.
* Up to **3 attempts** for transient failures (connection errors, HTTP 5xx) with
  an exponential backoff of 0.1s then 0.2s; 4xx responses are never retried and
  are returned to the caller.
* `OtterError` when transport keeps failing after all attempts.

There are no third-party dependencies anywhere in the SDK.

## HTTP capture

When the daemon sets `OTTER_CAPTURE_POLICY` to `metadata` or `full`, the SDK
installs process-local instrumentation before integration code is imported, so
requests made at import time are covered too. With any other value — or no value
at all — capture is off and nothing is installed.

The standard library `urllib` transport is always instrumented. `requests` and
`httpx` (both `Client` and `AsyncClient`) are instrumented as well when they are
importable in the run's interpreter; an optional client that is not installed is
never imported and never claimed. Each run reports the adapters it actually
installed, which is what `otter requests` shows as coverage.

Capture is diagnostic and never changes integration behaviour: it does not alter
a request's destination, suppress a write, or change an integration's return
values, exceptions or exit status. A failure to record is dropped rather than
raised. `metadata` records request summaries; `full` also records permitted
headers and bounded, sanitized JSON bodies. Redaction runs before delivery.

Reads that bypass the observed read path — `response.raw` in `requests`,
`response.stream` in `httpx` — custom `urllib` openers, clients with no adapter,
subprocesses and raw sockets are not captured.
[docs/http-capture.md](../../docs/http-capture.md) covers coverage, limits,
retention and redaction in full.

## Tests

The test suite needs neither the daemon nor any third-party package: it starts a
fake Otter API on an ephemeral port with `http.server`.

```bash
python3 -m unittest discover -s sdk/python/tests
```

It covers state JSON round-tripping (including 404 → default and `all()`), key
validation, trigger parsing/lazy fetching, client retry behaviour on 500 versus
no retry on 404, stderr fallback for logging, `OtterError` on a missing
`OTTER_API_URL`, and the `@run` decorator's success/failure/guard behaviour in a
real subprocess.

## Examples

The runtime ships no example catalog: `otter init` generates a working
integration that uses this SDK, and `make smoke` in the runtime repository
exercises it end to end. [docs/examples.md](../../docs/examples.md) covers the
manifest patterns — manual, cron-scheduled and webhook-triggered — that an
integration is built from.
