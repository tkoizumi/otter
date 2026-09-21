# HTTP API Reference

The daemon serves a JSON API on `--listen` (default `127.0.0.1:7337`). The CLI
is a thin client for this same API, so anything `otter` can do you can do with
`curl`.

- [Conventions](#conventions)
- [Authentication](#authentication)
- [Error responses](#error-responses)
- [Health](#health)
- [Integrations](#integrations)
- [Identity and lifecycle](#identity-and-lifecycle)
- [Runs](#runs)
- [Run logs](#run-logs)
- [Cancellation](#cancellation)
- [State](#state)
- [Webhooks](#webhooks)
- [End-to-end curl walkthrough](#end-to-end-curl-walkthrough)

## Conventions

- Base URL: `http://127.0.0.1:7337` unless you changed `--listen`.
- All request and response bodies are JSON (`Content-Type: application/json`).
  The one exception is `PUT` state, which accepts any JSON value.
- All timestamps are RFC 3339 UTC, e.g. `"2024-06-01T12:00:03Z"`.
- Durations in responses are integers in **seconds** (`timeout_seconds`), because
  machine consumers prefer them to strings.
- Ids: an integration id is the durable identity the runtime mints, not the
  manifest label. Run ids look like
  `run_01HZY3QW8K2M4P6R8T0V2X4Z6B`.
- An unknown path returns `404` with the standard error envelope; an unsupported
  method on a known path returns `405`.

For every example below:

```bash
export OTTER_API_URL=http://127.0.0.1:7337
export OTTER_API_TOKEN=paste-your-daemon-token-here   # omit if bound to loopback
```

and the helper the examples use to attach the bearer token:

```bash
auth() { [ -n "$OTTER_API_TOKEN" ] && printf 'Authorization: Bearer %s' "$OTTER_API_TOKEN"; }
```

## Authentication

There are three credential types, with different audiences.

| Credential | Header | Used by | Scope |
| --- | --- | --- | --- |
| Daemon API token | `Authorization: Bearer <token>` | CLI, operators, automation | The whole control-plane API. |
| Run state token | `Authorization: Bearer <run token>` | Child Python processes (the SDK) | Only the state and log endpoints for **that run's** integration. |
| Webhook token | `X-Otter-Token: <token>` or `?token=<token>` | External systems calling a hook | Only `POST /v1/hooks/{integration}` for one integration. |

### Loopback vs non-loopback

- **Default binding is loopback** (`127.0.0.1:7337`). A loopback API serves only
  local processes, so it does not require a token: requests without an
  `Authorization` header are accepted.
- **Binding to a non-loopback address requires `OTTER_API_TOKEN`.** Start the
  daemon with `--listen 0.0.0.0:7337` (or any non-loopback host) and no token and
  it **refuses to start**:

  ```
  FATA refusing to start: --listen 0.0.0.0:7337 is not a loopback address and
       OTTER_API_TOKEN is not set; set OTTER_API_TOKEN or bind to 127.0.0.1
  ```

- When `OTTER_API_TOKEN` **is** set, it is required on every `/v1` request even
  on loopback. A missing or wrong token returns `401`. Never expose an
  unauthenticated daemon to a network: `POST /v1/integrations/{id}/runs` is
  arbitrary code execution on the host.

### Run state tokens

When the executor starts a child process it mints a short-lived token for that
run and puts it in `OTTER_STATE_TOKEN`. The SDK uses it for state read/write and
log writes for that run's integration. The token is scoped to one integration
and one run: it can address the state endpoints for its own integration and the
log endpoints for its own run, and nothing else. Control-plane operations
(starting runs, cancelling runs, listing integrations or runs) reject it with
`403`, which is why the Python SDK needs no daemon token and why a leaked token
from one integration cannot drive another.

| Endpoint | Admin token | Run state token |
| --- | --- | --- |
| `GET /health` | yes | yes (open on loopback) |
| `GET /v1/integrations`, `GET /v1/runs` | yes | **no** (`403`) |
| `POST /v1/integrations/{id}/runs`, `POST /v1/runs/{id}/cancel` | yes | **no** (`403`) |
| `POST /v1/reload` | yes | **no** (`403`) |
| `GET /v1/integrations/{id}` | any integration | only its own integration |
| `GET /v1/runs/{id}`, `GET/POST /v1/runs/{id}/logs` | any run | only its own run |
| `GET/PUT/DELETE /v1/integrations/{id}/state[/{key}]` | any integration | only its own integration |
| `POST /v1/hooks/{integration}` | n/a — webhook token only | n/a |

### Webhook tokens

Each integration with `trigger.webhook.enabled: true` gets a token generated on
first start, persisted in SQLite, and returned by
`GET /v1/integrations/{id}` as `webhook_token`. Send it as `X-Otter-Token`, or as
a `?token=` query parameter when the caller cannot set headers. A bad or missing
token returns `401`; the hook is the one place a non-dashboard caller touches the
API, and it can only enqueue runs for the one integration whose token it holds.

## Error responses

Every error uses the same envelope:

```json
{
  "error": {
    "code": "not_found",
    "message": "no route for GET /v1/nope"
  }
}
```

| Status | `code` values | When |
| --- | --- | --- |
| `400` | `invalid_request` | Malformed body, invalid query parameter, invalid state key or value, body over the 1 MiB read limit. |
| `401` | `unauthorized` | Missing or wrong bearer token, or missing/wrong webhook token. |
| `403` | `forbidden` | Valid credential without permission for the target (a run state token used for another integration or run, or any run token on a control-plane endpoint). |
| `404` | `not_found` | Unknown path, unknown integration or run, unset state key, or a hook for an integration without a webhook trigger. |
| `405` | *(empty body)* | Known path, unsupported method — the router answers this itself. |
| `409` | `conflict` | Cancel on a run that is already terminal. |
| `500` | `internal_error` | Unexpected server error; details are in the daemon log. |
| `503` | `unavailable` | The daemon is shutting down and is not accepting new work. |

Error `message` strings are for humans; branch on `code`. Note that the same
`code` covers several conditions (there is one `not_found` for paths,
integrations, runs and state keys), so use the status plus the endpoint to
disambiguate.

## Health

### `GET /health`

Always answers `200`, so supervisors and container health checks can use it
without credentials. It reports liveness, not deep dependency health (there are
no external dependencies to check).

No parameters, no body.

```bash
curl -s "$OTTER_API_URL/health"
```

An **unauthenticated** caller — which on a loopback deployment means any local
caller, since no token is configured — receives the full payload:

```json
{
  "status": "ok",
  "version": "0.4.1",
  "uptime_seconds": 81234.5,
  "integrations": {"total": 7, "valid": 6, "invalid": 1},
  "queue_depth": 2,
  "runs": {"queued": 2, "running": 2, "succeeded": 1043, "failed": 17, "retrying": 1, "cancelled": 0, "timed_out": 3}
}
```

When an API token **is** configured and the request carries no token or a wrong
one, the response is a minimal liveness payload with the operational counters
omitted, so a token-protected deployment does not disclose integration and run
counts to the network:

```json
{"status": "ok", "version": "0.4.1", "uptime_seconds": 81234.5}
```

`integrations` reports how many manifests were discovered and how many of them
are valid, `queue_depth` is the number of runs waiting to be claimed, and `runs`
is a count per status. `status` is `ok` whenever the process is serving
requests. A `200` means the process is up and SQLite is readable; there is no
failure status from this endpoint by design, because a health check should
distinguish "the daemon answered" from "the daemon is gone", and the daemon does
not take the listener down until it has already stopped accepting work.

`otter status` reads this endpoint. If the counters are missing it prints a
hint that the daemon requires a token, which is how a typo in
`OTTER_API_TOKEN` becomes visible instead of silent.

The systemd examples in [operations.md](operations.md) poll this endpoint.

## Integrations

### `GET /v1/integrations`

List every integration found under the integrations root, **including invalid
ones** (so the API is a superset of `otter integrations`, which prints only valid
names unless `--all` is passed).

No parameters.

```bash
curl -s -H "$(auth)" "$OTTER_API_URL/v1/integrations"
```

```json
{
  "integrations": [
    {
      "id": "shopify-to-erp",
      "name": "shopify-to-erp",
      "description": "Sync Shopify orders into ERP.",
      "path": "/srv/otter/integrations/shopify-to-erp",
      "entrypoint": "main.py",
      "python_executable": "python3",
      "timeout_seconds": 300,
      "concurrency": 1,
      "retry": {
        "attempts": 5,
        "max_attempts": 5,
        "backoff": "exponential",
        "initial_delay": "2s",
        "max_delay": "60s"
      },
      "env": {"SHOPIFY_STORE": "example.myshopify.com"},
      "secrets": ["SHOPIFY_TOKEN", "ERP_TOKEN"],
      "triggers": {
        "cron": "*/5 * * * *",
        "webhook_enabled": true,
        "webhook_url": "http://127.0.0.1:7337/v1/hooks/shopify-to-erp"
      },
      "valid": true,
      "next_run_at": "2024-06-01T12:35:00Z"
    },
    {
      "id": "broken-one",
      "name": "broken-one",
      "path": "/srv/otter/integrations/broken-one",
      "entrypoint": "main.py",
      "python_executable": "python3",
      "timeout_seconds": 300,
      "concurrency": 1,
      "retry": {
        "attempts": 0,
        "max_attempts": 1,
        "backoff": "exponential",
        "initial_delay": "2s",
        "max_delay": "60s"
      },
      "triggers": {"webhook_enabled": false},
      "valid": false,
      "error": "entrypoint \"main.py\" not found in /srv/otter/integrations/broken-one"
    }
  ]
}
```

Field notes:

- `retry.attempts` is what the manifest declared; `max_attempts` is the effective
  total including the first attempt (`attempts: 0` becomes `max_attempts: 1`).
- Durations in `retry` are strings, since they come straight from the manifest.
- `next_run_at` appears only for integrations with a cron trigger.
- `webhook_url` appears when the webhook trigger is enabled.
- **`webhook_token` is never included in this listing** — only in the
  single-integration response, so a list call cannot spill credentials.

### `GET /v1/integrations/{id}`

Full detail for one integration. This is where you read the **webhook token**.

| Parameter | In | Description |
| --- | --- | --- |
| `id` | path | Integration `name` from the manifest. |

```bash
curl -s -H "$(auth)" "$OTTER_API_URL/v1/integrations/shopify-to-erp"
```

```json
{
  "id": "shopify-to-erp",
  "name": "shopify-to-erp",
  "description": "Sync Shopify orders into ERP.",
  "path": "/srv/otter/integrations/shopify-to-erp",
  "entrypoint": "main.py",
  "python_executable": "python3",
  "timeout_seconds": 300,
  "concurrency": 1,
  "retry": {
    "attempts": 5,
    "max_attempts": 5,
    "backoff": "exponential",
    "initial_delay": "2s",
    "max_delay": "60s"
  },
  "env": {"SHOPIFY_STORE": "example.myshopify.com"},
  "secrets": ["SHOPIFY_TOKEN", "ERP_TOKEN"],
  "triggers": {
    "cron": "*/5 * * * *",
    "webhook_enabled": true,
    "webhook_url": "http://127.0.0.1:7337/v1/hooks/shopify-to-erp",
    "webhook_token": "whk_9f2c1d7a4b8e5061"
  },
  "valid": true,
  "next_run_at": "2024-06-01T12:35:00Z"
}
```

`env` values are returned as written in the manifest (with `${VAR}` unexpanded);
`secrets` returns names only, never values. `webhook_token` is absent when the
webhook trigger is disabled.

Errors: `404 not_found`.

### `POST /v1/reload`

Re-reads the integrations directory against the running daemon. This is the
restart-free alternative to bouncing `otterd` after adding or editing an
integration.

No request body. Responses are `200` with a summary of what changed.

| Field | Description |
| --- | --- |
| `added` | Integrations the daemon did not know about before. |
| `removed` | Integrations that are no longer in the directory. |
| `changed` | Known integrations whose manifest differs. |
| `invalid` | Integrations present but not runnable. |
| `total`, `valid` | Counts after the reload. |
| `runs_cancelled` | Queued runs of removed integrations that were ended. |

```bash
curl -s -X POST -H "$(auth)" "$OTTER_API_URL/v1/reload"
```

```json
{
  "added": ["shopify-to-netsuite"],
  "removed": [],
  "changed": ["shopify-to-erp"],
  "invalid": [],
  "total": 3,
  "valid": 3,
  "runs_cancelled": 0
}
```

The daemon is not restarted. The API listener, the worker pool and every
executing run are left alone, and a cron trigger whose expression did not change
keeps its next fire time. Only the registry, the per-integration concurrency
limits and the cron triggers are replaced.

Discovery happens before any shared state is touched, so a slow walk of a large
integrations directory is invisible to everything already running, and a failed
walk leaves the previous set intact rather than half-applied.

Reload does not stage a release: a newly visible integration still answers `409`
from `POST /v1/integrations/{id}/runs` until `otter release` has run. Removing an
integration ends its **queued** runs, counted in `runs_cancelled`; runs already
executing are allowed to finish.

Errors: `409 conflict` (a reload is already in progress), `403 forbidden` (the
caller is not an admin), `500 internal_error` (the integrations directory could
not be read).

### `POST /v1/integrations/{id}/runs`

Start a manual run. This bypasses triggers entirely — it works for cron-only,
webhook-only and manual-only integrations alike. The run is **queued**, not run
inline: `--workers` and the integration's `concurrency` still apply.

| Parameter | In | Description |
| --- | --- | --- |
| `id` | path | Integration `name`. |
| `body` | JSON body, optional | `{"body": <any JSON>, "headers": {"X-Requested-By": "ops"}}` attached to the run's `metadata` and exposed as `ctx.trigger`. Omit it (or send `{}`) for a plain manual run. |

```bash
curl -s -X POST -H "$(auth)" -H 'Content-Type: application/json' \
  -d '{"body":{"reason":"manual backfill"},"headers":{"X-Requested-By":"ops"}}' \
  "$OTTER_API_URL/v1/integrations/shopify-to-erp/runs"
```

The response is deliberately minimal — enough to track the run, nothing more.
`202 Accepted` (not `200`): the run is queued, not finished.

```json
{
  "run_id": "run_01HZY7Q1W2E3R4T5Y6U7I8O9P0",
  "status": "queued"
}
```

Fetch `GET /v1/runs/{run_id}` for the full record. `otter run <integration>`
prints only the `run_id`, which makes it scriptable:

```bash
RUN_ID=$(otter run shopify-to-erp)
otter run-status "$RUN_ID"
```

Errors: `404 not_found`, `400 invalid_request` (the integration is invalid and
cannot be run, or the body is not valid JSON), `403 forbidden` (a run state token
was used), `503 unavailable`.

## Identity and lifecycle

An integration is addressed by its durable identity. The CLI resolves a label,
a path or an explicit `id:` reference through the daemon, so it never reads the
registry itself.

### `GET /v1/integrations/resolve`

Resolve a reference to the integration it names.

| Parameter | Description |
| --- | --- |
| `ref` | A label, a filesystem path, or `id:<id>`. Required. |

Returns the integration view. A reference that matches nothing is `404`; a label
carried by more than one active integration is `409` with the candidate ids and
paths in the message.

```bash
curl -s -H "$(auth)" "$OTTER_API_URL/v1/integrations/resolve?ref=counter"
```

### `POST /v1/integrations`

Register a source directory explicitly. Idempotent when the binding already
matches; it clears a deletion suppression and mints a fresh identity.

```json
{"path": "/srv/otter/integrations/counter"}
```

Errors: `400 invalid_request` (the path has no valid manifest),
`409 conflict` (the path is owned with a different marker; use reset).

### `POST /v1/integrations/{id}/reset`

Retire the identity and mint a fresh one at the same path. The old identity's
data is kept for inspection or deletion, and its release is not reused.

```json
{"old_id": "counter", "new_id": "0195a7c2-...", "name": "counter", "path": "/srv/otter/integrations/counter"}
```

Errors: `400 invalid_request`, `404 not_found`.

### `POST /v1/integrations/{id}/move`

Preserve an identity across a same-filesystem directory rename.

```json
{"destination": "/srv/otter/integrations/counter-v2"}
```

Errors: `400 invalid_request`, `404 not_found`, `409 conflict` (the destination
already exists or is owned, the paths nest, or the identity is not active).

### `DELETE /v1/integrations/{id}`

Purge the identity's state, run history and logs, queue rows, webhook token and
releases. Source files are left in place and the path is suppressed so a scan
cannot silently re-register it. The identity row survives as a tombstone and is
never reused.

```json
{"deleted": true, "integration": "counter"}
```

Errors: `400 invalid_request`, `404 not_found`.

## Runs

### `GET /v1/runs`

List runs, newest first.

| Query parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `integration_id` | string | unset | Filter to one integration. |
| `status` | string | unset | One of `queued`, `running`, `succeeded`, `failed`, `retrying`, `cancelled`, `timed_out`. An unknown value is a `400`. |
| `parent_run_id` | string | unset | Return the attempts that retry a given run — the rest of a retry chain. |
| `limit` | integer | `50` | Maximum rows to return; must be a positive integer. |
| `offset` | integer | `0` | Skip this many rows, for paging. Must be `>= 0`. |

```bash
curl -s -H "$(auth)" "$OTTER_API_URL/v1/runs?integration_id=shopify-to-erp&status=failed&limit=20"
```

```json
{
  "runs": [
    {
      "id": "run_01HZY7Q1W2E3R4T5Y6U7I8O9P0",
      "integration_id": "shopify-to-erp",
      "trigger_type": "cron",
      "status": "failed",
      "attempt": 1,
      "parent_run_id": null,
      "created_at": "2024-06-01T12:30:00Z",
      "started_at": "2024-06-01T12:30:00Z",
      "finished_at": "2024-06-01T12:30:07Z",
      "exit_code": 1,
      "error": "process exited with code 1",
      "metadata": {"type": "cron", "timeout_seconds": 300, "scheduled_at": "2024-06-01T12:30:00Z"}
    }
  ]
}
```

`created_at` is when the run record was created, which for a queued run is
equivalently when it was enqueued. The trigger payload, the effective timeout and
the scheduled time live inside `metadata`. A run that has not started yet has
`started_at: null` and `exit_code: null`.

Errors: `400 invalid_request` for an unknown `status` value or a non-numeric
`limit`.

### `GET /v1/runs/{id}`

One run, plus its whole retry chain.

| Parameter | In | Description |
| --- | --- | --- |
| `id` | path | Run id. |

```bash
curl -s -H "$(auth)" "$OTTER_API_URL/v1/runs/run_01HZY7Q1W2E3R4T5Y6U7I8O9P0"
```

```json
{
  "id": "run_01HZY7Q1W2E3R4T5Y6U7I8O9P0",
  "integration_id": "shopify-to-erp",
  "trigger_type": "cron",
  "status": "retrying",
  "attempt": 2,
  "parent_run_id": "run_01HZY6AAA111",
  "created_at": "2024-06-01T12:35:02Z",
  "started_at": "2024-06-01T12:35:02Z",
  "finished_at": "2024-06-01T12:35:09Z",
  "exit_code": 1,
  "error": "process exited with code 1",
  "metadata": {"type": "cron", "timeout_seconds": 300, "scheduled_at": "2024-06-01T12:35:00Z"},
  "root_run_id": "run_01HZY6AAA111",
  "latest_status": "retrying",
  "attempts": [
    {"id": "run_01HZY6AAA111", "attempt": 1, "status": "failed",   "exit_code": 1, "created_at": "2024-06-01T12:30:00Z", "started_at": "2024-06-01T12:30:00Z", "finished_at": "2024-06-01T12:30:07Z", "error": "process exited with code 1"},
    {"id": "run_01HZY7Q1W2E3R4T5Y6U7I8O9P0", "attempt": 2, "status": "retrying", "exit_code": 1, "created_at": "2024-06-01T12:35:02Z", "started_at": "2024-06-01T12:35:02Z", "finished_at": "2024-06-01T12:35:09Z", "error": "process exited with code 1"}
  ]
}
```

Field notes:

- `status` is the status of **this attempt**. `retrying` means the attempt
  finished but its backoff delay has not elapsed yet.
- `root_run_id`, `latest_status` and `attempts` are **derived** from the retry
  chain by walking `parent_run_id` to the root and back down, so they appear on
  this endpoint only, not in the list response.
- `latest_status` is the status of the newest attempt in the chain, which is what
  you usually want to display for `root_run_id`.
- `attempts` covers the entire chain, oldest first, including the run itself.
  Each element is a full run record, so it carries `metadata` too.
- `exit_code` is `null` while queued/running; when a process is killed by a
  signal the exit code is whatever the OS reports.
- `metadata` is `null` when the run was created without a trigger payload.

Errors: `404 not_found`.

## Run logs

### `GET /v1/runs/{id}/logs`

Captured output, oldest first. The SDK reads this endpoint too (it is how
`ctx.log` is not involved — the SDK *writes* logs; this endpoint reads them).

| Query parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `after_id` | integer | `0` | Return only log lines with `id` greater than this cursor. Must be `>= 0`. |
| `limit` | integer | `1000` | Maximum lines; must be a positive integer. |

```bash
curl -s -H "$(auth)" "$OTTER_API_URL/v1/runs/run_01HZY7Q1W2E3R4T5Y6U7I8O9P0/logs?after_id=0&limit=100"
```

```json
{
  "logs": [
    {"id": 1, "run_id": "run_01HZY7Q1W2E3R4T5Y6U7I8O9P0", "stream": "otter",  "message": "starting python3 main.py (timeout 300s)", "timestamp": "2024-06-01T12:35:02Z"},
    {"id": 2, "run_id": "run_01HZY7Q1W2E3R4T5Y6U7I8O9P0", "stream": "stdout", "message": "Starting sync",                              "timestamp": "2024-06-01T12:35:02Z"},
    {"id": 3, "run_id": "run_01HZY7Q1W2E3R4T5Y6U7I8O9P0", "stream": "stderr", "message": "urllib3: retrying request (attempt 1)",      "timestamp": "2024-06-01T12:35:04Z"},
    {"id": 4, "run_id": "run_01HZY7Q1W2E3R4T5Y6U7I8O9P0", "stream": "otter",  "message": "process exited with code 1",                 "timestamp": "2024-06-01T12:35:09Z"}
  ]
}
```

`stream` is one of:

| Stream | Origin |
| --- | --- |
| `stdout` | The child's standard output, one entry per line. |
| `stderr` | The child's standard error, one entry per line. |
| `otter` | Runtime annotations and SDK log calls: process start, timeout, cancellation, exit code, missing secret. |

`otter logs <run-id> --follow` polls this endpoint with an increasing `after_id`
until the run reaches a terminal status.

Errors: `404 not_found`, `400 invalid_request` for a negative `after_id` or a
non-positive `limit`.

### `POST /v1/runs/{id}/logs`

Used by the Python SDK to append runtime log entries from inside an integration
(`ctx.log.info(...)`). Authenticate with the **run state token** from
`OTTER_STATE_TOKEN`, not the daemon token.

```bash
curl -s -X POST -H "Authorization: Bearer $OTTER_STATE_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"stream":"otter","message":"checkpoint advanced to 4242","fields":{"level":"info"}}' \
  "$OTTER_API_URL/v1/runs/$OTTER_RUN_ID/logs"
```

Request body:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `message` | string | **yes** | One line; must not be empty. Over-long messages are truncated and suffixed `…(truncated)`. |
| `stream` | string | no | `stdout`, `stderr` or `otter`; defaults to `otter`. |
| `fields` | object | no | Structured fields for the entry. The SDK carries severity here under `level` (`debug`, `info`, `warning`, `error`) because the endpoint has no separate severity column. |

Response: `201 Created`.

```json
{"status": "recorded"}
```

Errors: `401 unauthorized`, `403 forbidden` (token does not belong to this run),
`404 not_found`, `400 invalid_request` (non-object body, empty `message`, or an
unknown `stream`).

## Cancellation

### `POST /v1/runs/{id}/cancel`

Stop a queued, running or retrying run. Queued runs are removed from the queue;
running runs get `SIGTERM` to their process group, then `SIGKILL` after about 5
seconds; retrying runs are cancelled before their next attempt starts.

Cancellation is **terminal and never retried**.

```bash
curl -s -X POST -H "$(auth)" "$OTTER_API_URL/v1/runs/run_01HZY7Q1W2E3R4T5Y6U7I8O9P0/cancel"
```

```json
{
  "run_id": "run_01HZY7Q1W2E3R4T5Y6U7I8O9P0",
  "status": "cancelled"
}
```

Ctrl-C on a foreground `otterd` (or `systemctl stop`) cancels
nothing — it triggers graceful shutdown instead, which waits for running
integrations and only then terminates them. Use this endpoint when you want a
specific run to stop now.

Errors: `404 not_found`, `409 conflict` (the run is already `succeeded`,
`failed`, `cancelled` or `timed_out`), `403 forbidden`.

## State

State is a per-integration JSON key/value store, durable in SQLite, readable and
writable both from Python (`ctx.state`) and over HTTP. Values are arbitrary
JSON. Keys must match `[A-Za-z0-9._:-]{1,128}`.

The SDK uses a run state token for these endpoints; operators use the daemon
token. Both are accepted, and the daemon token can address any integration.

### `GET /v1/integrations/{id}/state`

Return the whole state map.

```bash
curl -s -H "$(auth)" "$OTTER_API_URL/v1/integrations/shopify-to-erp/state"
```

```json
{
  "state": {
    "cursor": "123",
    "last_processed_customer_id": 4242,
    "stats": {"runs": 17, "errors": 0}
  }
}
```

`state` is a real JSON object: string, number, boolean, `null`, array and object
values all round-trip unchanged. An integration with no state returns
`{"state": {}}`.

Errors: `404 not_found`, `403 forbidden` (a run state token for another
integration).

### `GET /v1/integrations/{id}/state/{key}`

Read one key. **The response body is the raw JSON value with no envelope**, which
is what makes `otter state get <integration> <key>` printable verbatim:

```bash
curl -s -H "$(auth)" "$OTTER_API_URL/v1/integrations/shopify-to-erp/state/cursor"
```

```json
"123"
```

A number round-trips as a number (`12`), an object as an object
(`{"runs":17}`); there is no double encoding. The `Content-Type` is
`application/json; charset=utf-8`.

Errors: `404 not_found` (integration or key missing), `400 invalid_request` (the
key does not match `[A-Za-z0-9._:-]{1,128}`), `403 forbidden`.

### `PUT /v1/integrations/{id}/state/{key}`

Create or replace one key. This is an upsert, so it works for new and existing
keys.

```bash
curl -s -X PUT -H "$(auth)" -H 'Content-Type: application/json' \
  -d '"123"' "$OTTER_API_URL/v1/integrations/shopify-to-erp/state/cursor"
```

Request body: any JSON value. To store the string `123` send `"123"` (with
quotes); to store the number 123 send `123`.

```json
{
  "integration_id": "shopify-to-erp",
  "key": "cursor",
  "value": "123",
  "updated_at": "2024-06-01T12:40:11Z"
}
```

Errors: `400 invalid_request` (empty or invalid JSON body, or an invalid key),
`404 not_found`, `403 forbidden`.

### `DELETE /v1/integrations/{id}/state/{key}`

Delete one key.

```bash
curl -s -X DELETE -H "$(auth)" \
  "$OTTER_API_URL/v1/integrations/shopify-to-erp/state/cursor"
```

```json
{
  "integration_id": "shopify-to-erp",
  "key": "cursor",
  "deleted": true
}
```

Unlike a PUT, deleting a key that is not set returns `404`, so a cleanup script
that must be idempotent should treat `404` as success.

Errors: `400 invalid_request` (invalid key), `404 not_found`, `403 forbidden`.

## Webhooks

### `POST /v1/hooks/{integration}`

Enqueue a run from an external system. Requires the integration's webhook token
(`X-Otter-Token` header preferred, `?token=` accepted), **not** the daemon
token. Available only when `trigger.webhook.enabled: true` in the manifest; the
endpoint returns `404` otherwise, so disabled hooks are indistinguishable from
nonexistent ones.

| Parameter | In | Description |
| --- | --- | --- |
| `integration` | path | Integration `name`. |
| body | request body, optional | Recorded on the run's `metadata` and exposed as `ctx.trigger.body`. JSON is stored as-is; a non-JSON body is stored as a JSON string so `ctx.trigger.body` still returns something usable. |
| `token` | query, optional | Alternative to the header. |

```bash
curl -s -X POST \
  -H "X-Otter-Token: whk_9f2c1d7a4b8e5061" \
  -H 'Content-Type: application/json' \
  -d '{"order_id": 4242, "event": "order.created"}' \
  "$OTTER_API_URL/v1/hooks/order-events"
```

`202 Accepted` — the same minimal acknowledgement as a manual run:

```json
{
  "run_id": "run_01HZY8B2C3D4E5F6G7H8I9J0K1",
  "status": "queued"
}
```

A hook does not run the integration inline and does not wait for it: it enqueues
a run and returns immediately, so a slow integration cannot make a webhook caller
time out. The request headers are recorded on the run for `ctx.trigger.headers`,
**except** `Authorization` and `X-Otter-Token`, which are never persisted.

Errors:

| Status | `code` | Cause |
| --- | --- | --- |
| `401` | `unauthorized` | Missing or wrong `X-Otter-Token` / `token`. |
| `404` | `not_found` | Unknown integration, or its webhook trigger is disabled (deliberately identical, so the endpoint cannot enumerate integrations). |
| `403` | `forbidden` | A daemon or run token was used on the hook route. |
| `400` | `invalid_request` | Empty integration path segment. |
| `500` | `internal_error` | The run could not be enqueued. |

## End-to-end curl walkthrough

```bash
export OTTER_API_URL=http://127.0.0.1:7337
auth() { [ -n "$OTTER_API_TOKEN" ] && printf 'Authorization: Bearer %s' "$OTTER_API_TOKEN"; }

# 1. Is it alive?
curl -s "$OTTER_API_URL/health"

# 2. What integrations exist?
curl -s -H "$(auth)" "$OTTER_API_URL/v1/integrations" | python3 -m json.tool

# 3. Start one manually and capture the run id.
RUN_ID=$(curl -s -X POST -H "$(auth)" -H 'Content-Type: application/json' -d '{}' \
  "$OTTER_API_URL/v1/integrations/shopify-to-erp/runs" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["run_id"])')
echo "$RUN_ID"

# 4. Watch it.
sleep 2
curl -s -H "$(auth)" "$OTTER_API_URL/v1/runs/$RUN_ID" | python3 -m json.tool

# 5. Read its output.
curl -s -H "$(auth)" "$OTTER_API_URL/v1/runs/$RUN_ID/logs?after_id=0&limit=500" \
  | python3 -c 'import json,sys
for row in json.load(sys.stdin)["logs"]:
    print(f"{row[\"stream\"]:>6} | {row[\"message\"]}")'

# 6. Inspect and mutate state.
curl -s -H "$(auth)" "$OTTER_API_URL/v1/integrations/shopify-to-erp/state"
curl -s -X PUT -H "$(auth)" -H 'Content-Type: application/json' \
  -d '"2024-06-01T00:00:00Z"' \
  "$OTTER_API_URL/v1/integrations/shopify-to-erp/state/backfill_after"

# 7. Cancel it if it is still going.
curl -s -X POST -H "$(auth)" "$OTTER_API_URL/v1/runs/$RUN_ID/cancel"

# 8. Fire a webhook.
TOKEN=$(curl -s -H "$(auth)" "$OTTER_API_URL/v1/integrations/order-events" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["webhook_token"])')
curl -s -X POST -H "X-Otter-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"order_id": 4242}' "$OTTER_API_URL/v1/hooks/order-events"
```

The same actions through the CLI:

```bash
otter status
otter integrations
otter inspect shopify-to-erp
otter run shopify-to-erp
otter runs --integration shopify-to-erp --status failed --limit 20
otter run-status <run-id>
otter logs <run-id> --follow
otter state get shopify-to-erp cursor
otter state set shopify-to-erp cursor '"123"'
otter state delete shopify-to-erp cursor
otter validate my-integration
```

Add `--json` to any CLI command for raw JSON instead of the human-readable
default, and `--api`/`--token` to target a different daemon:

```bash
otter --api https://otter.internal.example.com --token "$OTTER_API_TOKEN" \
  --json runs --status failed --limit 5
```
