# HTTP API Reference

The daemon serves a JSON API on `--listen` (default `127.0.0.1:7337`). The CLI
is a thin client for this same API, so anything `otter` can do you can do with
`curl`.

- [Conventions](#conventions)
- [Authentication](#authentication)
- [Error responses](#error-responses)
- [Health](#health)
- [Jobs](#jobs)
- [Identity and lifecycle](#identity-and-lifecycle)
- [Runs](#runs)
- [Run logs](#run-logs)
- [HTTP request inspection](#http-request-inspection)
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
- Ids: a job id is the durable identity the runtime mints, not the
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

There are four credential types, with different audiences.

| Credential | Header | Used by | Scope |
| --- | --- | --- | --- |
| Daemon API token | `Authorization: Bearer <token>` | CLI, operators, automation | The whole control-plane API. |
| Scoped API token | `Authorization: Bearer <token>` | A gateway, a control plane, a client's backend | `read`, `control` or `capture`; see [Scoped API tokens](#scoped-api-tokens). |
| Run state token | `Authorization: Bearer <run token>` | Child Python processes (the SDK) | The state, log and capture-ingestion endpoints for **that run's** job and run. |
| Webhook token | `X-Otter-Token: <token>` or `?token=<token>` | External systems calling a hook | Only `POST /v1/hooks/{job}` for one job. |

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
  unauthenticated daemon to a network: `POST /v1/jobs/{id}/runs` is
  arbitrary code execution on the host.

### Run state tokens

When the executor starts a child process it mints a short-lived token for that
run and puts it in `OTTER_STATE_TOKEN`. The SDK uses it for state read/write and
log writes for that run's job. The token is scoped to one job
and one run: it can address the state endpoints for its own job and the
log endpoints for its own run, and nothing else. Control-plane operations
(starting runs, cancelling runs, listing jobs or runs) reject it with
`403`, which is why the Python SDK needs no daemon token and why a leaked token
from one job cannot drive another.

| Endpoint | Admin token | Run state token |
| --- | --- | --- |
| `GET /health` | yes | yes (open on loopback) |
| `GET /v1/jobs`, `GET /v1/runs` | yes | **no** (`403`) |
| `POST /v1/jobs/{id}/runs`, `POST /v1/runs/{id}/cancel` | yes | **no** (`403`) |
| `POST /v1/reload` | yes | **no** (`403`) |
| `GET /v1/jobs/{id}` | any job | only its own job |
| `GET /v1/runs/{id}`, `GET/POST /v1/runs/{id}/logs` | any run | only its own run |
| `POST /v1/runs/{id}/requests/events` | any run | only its own run |
| `GET /v1/runs/{id}/requests[/{request_id}]` | any run | **no** (`403`) |
| `GET /v1/runs/{id}/timeline` | any run | **no** (`403`) |
| `GET /v1/requests/{request_id}` | any run | **no** (`403`) |
| `GET/PUT/DELETE /v1/jobs/{id}/state[/{key}]` | any job | only its own job |
| `POST /v1/hooks/{job}` | n/a — webhook token only | n/a |

### Scoped API tokens

The daemon API token is all-or-nothing. The same credential reads business
state, reads capture payloads, registers and deletes jobs, and executes
arbitrary code through `POST /v1/jobs/{id}/runs` — so a gateway that commands a
runtime on a customer's behalf should not hold it. Mint it a narrower one.

```bash
otter token create --name cloud-gateway --scope control
otter token list
otter token revoke <id>
```

The token is printed **once**, at creation. The daemon stores only a SHA-256 of
it, so a lost token is replaced rather than recovered. Revocation takes effect
on the next request: there is no cache to expire and no restart.

| Scope | Can |
| --- | --- |
| `read` | Read jobs, runs, run output, the merged timeline, and capture **summaries**. |
| `control` | Everything `read` can, plus run, cancel, pause, resume, and create, change, pause, resume or delete a schedule. |
| `capture` | Everything `control` can, plus read captured request and response **bodies** (the sanitized payloads, never the headers). |

`capture` is a superset of `control` because a control plane needs both from one
credential: the command surface that re-runs a job and the capture read that
explains it. It is deliberately **not** implied by `control`: reading the
client's traffic is a separate, explicitly named authority, so a credential
minted last week does not silently gain it. Mint one with
`otter token create --name cloud-capture --scope capture`.

No scope can read job state (`ctx.state`), register, reset, move or delete a
job, reload the daemon, or manage tokens. Those stay with the admin token. A
scoped token also never receives a job's `webhook_token`, which is itself a
credential that triggers runs.

| Endpoint | `read` | `control` | `capture` | Admin |
| --- | --- | --- | --- | --- |
| `GET /v1/jobs`, `GET /v1/runs` | yes | yes | yes | yes |
| `GET /v1/jobs/{id}` | yes (no `webhook_token`) | yes (no `webhook_token`) | yes (no `webhook_token`) | yes |
| `GET /v1/runs/{id}`, `GET /v1/runs/{id}/logs` | yes | yes | yes | yes |
| `GET /v1/runs/{id}/timeline` | yes, without HTTP entries | yes, without HTTP entries | yes, without HTTP entries | yes |
| `GET /v1/runs/{id}/requests` (summaries) | yes | yes | yes | yes |
| `POST /v1/jobs/{id}/runs`, `POST /v1/runs/{id}/cancel` | **no** (`403`) | yes | yes | yes |
| `POST /v1/jobs/{id}/pause`, `POST /v1/jobs/{id}/resume` | **no** (`403`) | yes | yes | yes |
| `GET /v1/jobs/{id}/schedules`, `GET /v1/schedules/{schedule_id}` | yes | yes | yes | yes |
| `GET /v1/jobs/{id}/config` | yes | yes | yes | yes |
| `PUT /v1/jobs/{id}/config` | **no** (`403`) | **no** (`403`) | **no** (`403`) | yes |
| `PUT`/`DELETE /v1/jobs/{id}/schedule` (deprecated) | **no** (`403`) | yes | yes | yes |
| `POST /v1/jobs/{id}/schedules`, `PATCH`/`DELETE /v1/schedules/{id}`, `POST /v1/schedules/{id}/pause\|resume` | **no** (`403`) | yes | yes | yes |
| `GET`/`PUT`/`DELETE /v1/jobs/{id}/state[/{key}]` | **no** (`403`) | **no** (`403`) | **no** (`403`) | yes |
| `GET /v1/runs/{id}/requests/{request_id}`, `GET /v1/requests/{request_id}` | **no** (`403`) | **no** (`403`) | yes | yes |
| `POST /v1/jobs`, `/reset`, `/move`, `DELETE /v1/jobs/{id}`, `POST /v1/reload` | **no** (`403`) | **no** (`403`) | **no** (`403`) | yes |
| `POST`/`GET`/`DELETE /v1/tokens` | **no** (`403`) | **no** (`403`) | **no** (`403`) | yes |

`GET /v1/runs/{id}/timeline` carries captured HTTP exchanges, which are business
data, so HTTP entries are included by default only for the admin token. A scoped
caller that passes `include_http=true` is refused with `403` rather than served a
silently narrowed page.

### Webhook tokens

Each job with `trigger.webhook.enabled: true` gets a token generated on
first start, persisted in SQLite, and returned by
`GET /v1/jobs/{id}` as `webhook_token`. Send it as `X-Otter-Token`, or as
a `?token=` query parameter when the caller cannot set headers. A bad or missing
token returns `401`; the hook is the one place a non-dashboard caller touches the
API, and it can only enqueue runs for the one job whose token it holds.

## Error responses

Every error uses the same envelope:

```json
{
  "schema_version": 1,
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
| `403` | `forbidden` | Valid credential without permission for the target (a run state token used for another job or run, or any run token on a control-plane endpoint). |
| `404` | `not_found` | Unknown path, unknown job or run, unset state key, or a hook for a job without a webhook trigger. |
| `405` | *(empty body)* | Known path, unsupported method — the router answers this itself. |
| `409` | `conflict` | Cancel on a run that is already terminal, or capture ingestion for a run with no capture configuration. |
| `500` | `internal_error` | Unexpected server error; details are in the daemon log. |
| `503` | `unavailable` | The daemon is shutting down and is not accepting new work, an autonomous trigger arrived for a paused job, or a bounded timeline read did not finish in time (see `GET /v1/runs/{id}/timeline`). |

Error `message` strings are for humans; branch on `code`. Note that the same
`code` covers several conditions (there is one `not_found` for paths,
jobs, runs and state keys), so use the status plus the endpoint to
disambiguate.

The error envelope is a frozen shape: [compatibility.md](compatibility.md)
states what may change and how a new `code` is added.

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
  "schema_version": 1,
  "status": "ok",
  "version": "v0.2.0",
  "uptime_seconds": 81234.5,
  "jobs": {"total": 7, "valid": 6, "invalid": 1},
  "queue_depth": 2,
  "runs": {"queued": 2, "running": 2, "succeeded": 1043, "failed": 17, "retrying": 1, "cancelled": 0, "timed_out": 3},
  "queue": {
    "oldest_waiting_at": "2026-10-01T04:02:11Z",
    "oldest_waiting_seconds": 3725.4,
    "by_job": {"shopify-to-erp": 2},
    "retrying": 1,
    "next_retry_at": "2026-10-01T05:14:00Z"
  },
  "freshness": [
    {"job_id": "9f1c...", "name": "shopify-to-erp", "last_success_at": "2026-10-01T05:00:04Z", "age_seconds": 325.1},
    {"job_id": "41ab...", "name": "nightly-report"}
  ],
  "storage": {"db_bytes": 812345678, "disk_free_bytes": 12884901888, "disk_total_bytes": 21474836480}
}
```

When an API token **is** configured and the request carries no token or a wrong
one, the response is a minimal liveness payload with every operational field
omitted, so a token-protected deployment does not disclose job, run, queue,
freshness or storage detail to the network:

```json
{"schema_version": 1, "status": "ok", "version": "v0.2.0", "uptime_seconds": 81234.5}
```

`jobs` reports how many manifests were discovered and how many of them
are valid, `queue_depth` is the number of runs waiting to be claimed, and `runs`
is a count per status.

The remaining blocks answer what counts cannot, and are present only for an
authenticated caller:

| Field | Meaning |
| --- | --- |
| `queue.oldest_waiting_at`, `queue.oldest_waiting_seconds` | Submission time and age of the oldest run that is claimable *now* (`available_at <= now`). Absent when nothing is claimable. |
| `queue.by_job` | Queue depth per job id. |
| `queue.retrying` | Runs parked by retry backoff — waiting on a clock, not on capacity, which is why they are not part of the age above. |
| `queue.next_retry_at` | When the soonest parked retry becomes claimable. |
| `freshness[]` | One entry per job: `last_success_at` is the newest succeeded run's completion time and `age_seconds` its age. Both are absent for a job that has never succeeded, which is itself the signal. Sorted by `job_id`. |
| `storage.db_bytes` | The live database's size, `PRAGMA page_count * page_size` (so it excludes the WAL file). |
| `storage.disk_free_bytes`, `storage.disk_total_bytes` | The data directory's filesystem, read daemon-side so a remote caller can watch it. Both are `0` on a platform that cannot report filesystem space. |

`status` is `ok` whenever the process is serving
requests. A `200` means the process is up and SQLite is readable; there is no
failure status from this endpoint by design, because a health check should
distinguish "the daemon answered" from "the daemon is gone", and the daemon does
not take the listener down until it has already stopped accepting work.

A block whose read fails is dropped from the response and logged, rather than
turning a liveness check into an error.

`otter status` reads this endpoint. If the counters are missing it prints a
hint that the daemon requires a token, which is how a typo in
`OTTER_API_TOKEN` becomes visible instead of silent.

The systemd examples in [operations.md](operations.md) poll this endpoint.

### `GET /v1/version`

The machine-readable contract document: the one request a script or a control
plane makes to learn the schema version, the runtime-contract version, the
manifest schema, the embedded Python SDK version and the supported platforms.
Like `/health`, it is **public** — a client must be able to learn the shape
before it authenticates to it.

```bash
curl -s "$OTTER_API_URL/v1/version"
otter --api "$OTTER_API_URL" --json version
```

```json
{
  "schema_version": 1,
  "contract_version": 2,
  "product_version": "v0.4.0",
  "manifest_schema": 1,
  "sdk_version": "0.1.0",
  "supported_platforms": ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
}
```

`schema_version` versions the JSON shapes themselves; it appears in every
object-shaped machine document (`/health`, this document, the error envelope,
`POST /v1/reload`, the schedule list). Its meaning, and what counts as a change
to it, is [compatibility.md](compatibility.md).

## Jobs

### `GET /v1/jobs`

List every job found under the jobs root, **including invalid
ones** (so the API is a superset of the live half of `otter jobs`, which hides
invalid jobs and non-active identities unless `--all` is passed).

No parameters.

```bash
curl -s -H "$(auth)" "$OTTER_API_URL/v1/jobs"
```

```json
{
  "schema_version": 1,
  "jobs": [
    {
      "id": "shopify-to-erp",
      "name": "shopify-to-erp",
      "description": "Sync Shopify orders into ERP.",
      "path": "/srv/otter/jobs/shopify-to-erp",
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
      "next_run_at": "2024-06-01T12:35:00Z",
      "last_success_at": "2024-06-01T12:30:04Z"
    },
    {
      "id": "broken-one",
      "name": "broken-one",
      "path": "/srv/otter/jobs/broken-one",
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
      "error": "entrypoint \"main.py\" not found in /srv/otter/jobs/broken-one"
    }
  ]
}
```

Field notes:

- `retry.attempts` is what the manifest declared; `max_attempts` is the effective
  total including the first attempt (`attempts: 0` becomes `max_attempts: 1`).
- Durations in `retry` are strings, since they come straight from the manifest.
- `next_run_at` appears only for jobs with a cron trigger.
- `last_success_at` is the completion time of the job's most recent succeeded
  run, and is absent for a job that has never succeeded. It is computed once for
  the whole listing, which is what lets `otter jobs --schedule` render freshness
  without listing runs once per job. Only this listing carries it;
  `GET /v1/jobs/{id}` does not.
- `webhook_url` appears when the webhook trigger is enabled.
- **`webhook_token` is never included in this listing** — only in the
  single-job response, so a list call cannot spill credentials.

### `GET /v1/jobs/{id}`

Full detail for one job. This is where you read the **webhook token**.

| Parameter | In | Description |
| --- | --- | --- |
| `id` | path | Job `name` from the manifest. |

```bash
curl -s -H "$(auth)" "$OTTER_API_URL/v1/jobs/shopify-to-erp"
```

```json
{
  "id": "shopify-to-erp",
  "name": "shopify-to-erp",
  "description": "Sync Shopify orders into ERP.",
  "path": "/srv/otter/jobs/shopify-to-erp",
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

A paused job adds `triggers.paused` and `triggers.paused_at`, and omits
`next_run_at`: a paused job has no next fire time. See
`POST /v1/jobs/{id}/pause`.

Errors: `404 not_found`.

### `POST /v1/reload`

Re-reads the jobs directory against the running daemon. This is the
restart-free alternative to bouncing `otterd` after adding or editing an
job.

No request body. Responses are `200` with a summary of what changed.

| Field | Description |
| --- | --- |
| `added` | Jobs the daemon did not know about before. |
| `removed` | Jobs that are no longer in the directory. |
| `changed` | Known jobs whose manifest differs. |
| `invalid` | Jobs present but not runnable. |
| `total`, `valid` | Counts after the reload. |
| `runs_cancelled` | Queued runs of removed jobs that were ended. |

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
keeps its next fire time. Only the registry, the per-job concurrency
limits and the cron triggers are replaced.

Discovery happens before any shared state is touched, so a slow walk of a large
jobs directory is invisible to everything already running, and a failed
walk leaves the previous set intact rather than half-applied.

Reload does not stage a release: a newly visible job still answers `409`
from `POST /v1/jobs/{id}/runs` until `otter release` has run. Removing an
job ends its **queued** runs, counted in `runs_cancelled`; runs already
executing are allowed to finish.

Errors: `409 conflict` (a reload is already in progress), `403 forbidden` (the
caller is not an admin), `500 internal_error` (the jobs directory could
not be read).

### `POST /v1/jobs/{id}/runs`

Start a manual run. This bypasses triggers entirely — it works for cron-only,
webhook-only and manual-only jobs alike. The run is **queued**, not run
inline: `--workers` and the job's `concurrency` still apply.

| Parameter | In | Description |
| --- | --- | --- |
| `id` | path | Job `name`. |
| `body` | JSON body, optional | `{"body": <any JSON>, "headers": {"X-Requested-By": "ops"}}` attached to the run's `metadata` and exposed as `ctx.trigger`. Omit it (or send `{}`) for a plain manual run. |
| `capture` | query | HTTP capture level for this run: `off`, `metadata` or `full`. Omitting it is not the same as naming a level: the job's `capture:` field applies, then the deployment's `--capture-default`, then the built-in default of `full`. The option is a query parameter, not part of the trigger body, so the body stays byte-for-byte the trigger JSON. An unknown value is a `400`. |

```bash
curl -s -X POST -H "$(auth)" -H 'Content-Type: application/json' \
  -d '{"body":{"reason":"manual backfill"},"headers":{"X-Requested-By":"ops"}}' \
  "$OTTER_API_URL/v1/jobs/shopify-to-erp/runs"
```

The response is deliberately minimal — enough to track the run, nothing more.
`202 Accepted` (not `200`): the run is queued, not finished.

```json
{
  "run_id": "run_01HZY7Q1W2E3R4T5Y6U7I8O9P0",
  "status": "queued"
}
```

Fetch `GET /v1/runs/{run_id}` for the full record. On a terminal, `otter run
<job>` waits for the whole retry chain to settle, then prints
`status: <status>` and the run's own output, exiting non-zero when the run
failed. A failed or timed-out attempt ends the chain once it has used the
job's last allowed attempt (`max_attempts` below); until then the
command keeps watching for the retry. When stdout is not a terminal, or with
`--no-wait` or `--json`, the command prints only the `run_id`, which makes it
scriptable:

```bash
RUN_ID=$(otter run shopify-to-erp)
otter run-status "$RUN_ID"
```

Errors: `404 not_found`, `400 invalid_request` (the job is invalid and
cannot be run, or the body is not valid JSON), `403 forbidden` (a run state token
was used), `503 unavailable`.

## Identity and lifecycle

A job is addressed by its durable identity. The CLI resolves a label,
a path or an explicit `id:` reference through the daemon, so it never reads the
registry itself.

### `GET /v1/jobs/resolve`

Resolve a reference to the job it names.

| Parameter | Description |
| --- | --- |
| `ref` | A label, a filesystem path, or `id:<id>`. Required. |

Returns the job view. A reference that matches nothing is `404`; a label
carried by more than one active job is `409` with the candidate ids and
paths in the message.

```bash
curl -s -H "$(auth)" "$OTTER_API_URL/v1/jobs/resolve?ref=counter"
```

### `POST /v1/jobs`

Register a source directory explicitly. Idempotent when the binding already
matches; it clears a deletion suppression and mints a fresh identity.

```json
{"path": "/srv/otter/jobs/counter"}
```

Errors: `400 invalid_request` (the path has no valid manifest),
`409 conflict` (the path is owned with a different marker; use reset).

### `POST /v1/jobs/{id}/reset`

Retire the identity and mint a fresh one at the same path. The old identity's
data is kept for inspection or deletion, and its release is not reused.

```json
{"old_id": "counter", "new_id": "0195a7c2-...", "name": "counter", "path": "/srv/otter/jobs/counter"}
```

Errors: `400 invalid_request`, `404 not_found`.

### `POST /v1/jobs/{id}/move`

Preserve an identity across a same-filesystem directory rename.

```json
{"destination": "/srv/otter/jobs/counter-v2"}
```

Errors: `400 invalid_request`, `404 not_found`, `409 conflict` (the destination
already exists or is owned, the paths nest, or the identity is not active).

### `POST /v1/jobs/{id}/pause`

Suspend a job's autonomous triggers. Cron stops firing and the webhook
refuses a trigger; the identity, state, run history, webhook token and releases
are untouched, and a manual run through `POST /v1/jobs/{id}/runs` still
works. No request body.

```json
{
  "job_id": "0195a7c2-...",
  "name": "shopify-to-erp",
  "paused": true,
  "changed": true,
  "since": "2024-06-01T12:30:00Z"
}
```

Pausing is idempotent: repeating it answers `200` with `changed: false`.

Errors: `404 not_found`, `409 conflict` (the identity is retired or deleted and
accepts no work at all).

### `POST /v1/jobs/{id}/resume`

Re-arm the triggers a pause suspended. The cron expression is taken from the
live manifest and the next fire time is computed from now; runs that were
missed while paused are **not** replayed. No request body.

```json
{
  "job_id": "0195a7c2-...",
  "name": "shopify-to-erp",
  "paused": false,
  "changed": true
}
```

Resuming a job that was not paused is a successful no-op with
`changed: false`.

Errors: `404 not_found`, `409 conflict`.

### Schedules

A schedule is **runtime state**, not manifest state: a cadence changes far more
often than a job's code, and a value that both a file and an API can write
eventually disagrees with itself. Every schedule carries an `origin`:

- `manifest` — reconciled from the job's `trigger.cron` on every reload. The
  file owns the row: `PATCH` and `DELETE` refuse it with `409`.
- `api` — created through this API. A reload never reads, changes or deletes it.

A job may hold any number of schedules from either origin, and they fire
independently. Every schedule's cron is interpreted in its own IANA `timezone`,
which defaults to **UTC** — a manifest's `trigger.cron` used to mean the host's
local time; it now means UTC (see
[manifest-reference.md](manifest-reference.md#cron)).

#### `GET /v1/jobs/{id}/schedules`

List a job's schedules.

```json
{
  "schedules": [
    {
      "id": "0195a7c2-3f10-...",
      "job_id": "0195a7c2-...",
      "name": "shopify-to-erp",
      "cron": "*/15 * * * *",
      "timezone": "UTC",
      "payload": {"dataset": 42},
      "missed_policy": "skip",
      "origin": "manifest",
      "next_run_at": "2026-10-01T12:45:00Z",
      "changed": false
    }
  ]
}
```

#### `POST /v1/jobs/{id}/schedules`

Create an `api`-owned schedule.

```json
{"cron": "*/15 * * * *", "timezone": "Europe/London", "payload": {"dataset": 42}, "missed_policy": "skip"}
```

`cron` is required. `timezone` defaults to `UTC`. `payload` is an optional JSON
object of at most 64 KiB; it becomes `ctx.trigger.body` for every run the
schedule starts. `missed_policy` must be `skip` (or absent) in `v0.4.0`:
`coalesce` and `catch_up` are reserved names and are refused with `400` rather
than accepted and silently ignored.

Send an `Idempotency-Key` header to make the command retry-safe: a second
`POST` with the same key returns the **existing** schedule with `changed: false`
and status `200` rather than creating a second one. A first create returns
`201`.

Errors: `400 invalid_request` (missing or unparseable cron, unknown timezone,
an oversized or non-object payload), `404 not_found`, `409 conflict`.

#### `GET /v1/schedules/{schedule_id}`

Return one schedule in the same shape as the list entry.

#### `PATCH /v1/schedules/{schedule_id}`

Change an `api`-owned schedule. Every field is optional; a field that is absent
is left alone.

```json
{"cron": "@hourly", "timezone": "UTC", "payload": {}, "missed_policy": "skip"}
```

An empty `cron` is refused (`400`) — delete the schedule instead. A
`manifest`-owned row is refused with `409`, naming the manifest as the owner.

#### `DELETE /v1/schedules/{schedule_id}`

Delete an `api`-owned schedule and its occurrence ledger. Returns `204` with no
body. A `manifest`-owned row is refused with `409`.

#### `POST /v1/schedules/{schedule_id}/pause` and `/resume`

Hold one schedule back, or release it, without touching the job's other
schedules and without pausing the job. Pausing is an operator control, so it is
accepted on a `manifest`-owned row too; a reload preserves `paused_at`.

### Job configuration

A job's configuration is the layer between the release (immutable code) and the
run (one accepted input): a store name, a dataset id, a backfill date. It is
**versioned**. Each write creates an immutable version and moves a pointer, and
every run records the `config_version` it resolved at submission, so a queued,
retrying or backlogged run keeps the values it was accepted with — exactly as it
keeps its release. Configuration values are **not secrets** and must never hold
one; a secret belongs in `otter.env`.

The child reads them as `ctx.config` (a dict; `{}` when the job has none). The
daemon passes the pinned version verbatim as `OTTER_CONFIG`; configuration is
deliberately *not* projected into named environment variables in `v0.4.0`, so a
configuration key can never silently shadow a manifest `env` entry.

#### `GET /v1/jobs/{id}/config`

```json
{
  "schema_version": 1,
  "job_id": "0195a7c2-...",
  "name": "shopify-to-erp",
  "config_version": "c1f0...",
  "values": {"dataset": 42},
  "updated_at": "2026-10-03T12:00:00Z",
  "updated_by": "admin"
}
```

An unconfigured job returns `"values": {}` and no `config_version`.

Errors: `404 not_found`.

#### `PUT /v1/jobs/{id}/config`

```json
{"values": {"dataset": 42}}
```

`values` must be a JSON object of at most 64 KiB. An absent `values` key is an
empty configuration, which is how a configuration is cleared. Setting the
current values again is a no-op with `changed: false`, so a deploy script can
apply configuration unconditionally. Already-accepted runs keep the version they
pinned.

**Admin-only in `v0.4.0`.** The `control` scope's published surface (`CL-21`)
does not include configuration, and widening a credential silently would break
its contract; see [compatibility.md](compatibility.md).

Errors: `400 invalid_request`, `404 not_found`, `409 conflict` (a retired
identity).

#### `PUT /v1/jobs/{id}/schedule` — deprecated

Replace a job's **single** cadence. This is the v0.3.0 surface. It refuses a job
whose cadence is manifest-owned with `409`; new callers use
`POST /v1/jobs/{id}/schedules` and the schedule-id endpoints above.

```json
{"cron": "*/15 * * * *"}
```

The response reports the stored cadence and, when one is armed, the next fire
time. `changed: false` means the value was already in force, so a deploy script
can apply it unconditionally.

An invalid expression is rejected **before** anything is written, so a failed
change leaves the previous cadence in force and armed.

Errors: `400 invalid_request`, `404 not_found`, `409 conflict` (a manifest-owned
or retired identity).

#### `DELETE /v1/jobs/{id}/schedule` — deprecated

Clear a job's single cadence. The job stops firing on its own;
`POST /v1/jobs/{id}/runs` still works.

Clearing is a first-class state rather than a return to the manifest default.
The row survives with an empty `cron`, so a later `otter reload` cannot
re-import `trigger.cron` and silently undo the decision. A manifest-owned
cadence is refused with `409`.

Errors: `404 not_found`, `409 conflict`.

### `DELETE /v1/jobs/{id}`

Purge the identity's state, run history and logs, queue rows, webhook token and
releases. Source files are left in place and the path is suppressed so a scan
cannot silently re-register it. The identity row survives as a tombstone and is
never reused.

```json
{"deleted": true, "job": "counter"}
```

Errors: `400 invalid_request`, `404 not_found`.

## Scoped tokens

The three endpoints behind [Scoped API tokens](#scoped-api-tokens). All three
require the admin token: a scoped credential must not be able to mint or revoke
one.

### `POST /v1/tokens`

Mints a token. The response is the only time the token itself is ever returned.

```json
{"name": "cloud-gateway", "scope": "control"}
```

`201`:

```json
{
  "id": "6f1c8a2e-...",
  "name": "cloud-gateway",
  "scope": "control",
  "created_at": "2026-10-02T12:00:00Z",
  "token": "otter_ctl_9f2c..."
}
```

`400` when the name is blank, or the scope is neither `read`, `control` nor
`capture`.

### `GET /v1/tokens`

Lists every token, newest first. Revoked tokens are included, so an operator can
see what used to have access and when it was withdrawn. No response reveals a
token.

```json
{"schema_version": 1, "tokens": [
  {"id": "6f1c8a2e-...", "name": "cloud-gateway", "scope": "control",
   "created_at": "2026-10-02T12:00:00Z"},
  {"id": "0a3d1b77-...", "name": "old-gateway", "scope": "read",
   "created_at": "2026-09-30T08:00:00Z", "revoked_at": "2026-10-02T10:00:00Z"}
]}
```

### `DELETE /v1/tokens/{id}`

Revokes a token. The next request that presents it is refused with `401`.

```json
{"id": "6f1c8a2e-...", "revoked": true}
```

Revoking an already-revoked token succeeds, because the operator asked for it to
be unusable and it is. Revoking an unknown id is `404`.

## Runs

### `GET /v1/runs`

List runs, newest first.

| Query parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `job_id` | string | unset | Filter to one job. Accepts a manifest label, a source path, or `id:<id>` -- resolved to the durable identity -- as well as the identity itself. An unknown reference is a `404`. |
| `status` | string | unset | One of `queued`, `running`, `succeeded`, `failed`, `retrying`, `cancelled`, `timed_out`. An unknown value is a `400`. |
| `parent_run_id` | string | unset | Return the attempts that retry a given run — the rest of a retry chain. |
| `limit` | integer | `50` | Maximum rows to return; must be a positive integer. |
| `offset` | integer | `0` | Skip this many rows, for paging. Must be `>= 0`. |

```bash
curl -s -H "$(auth)" "$OTTER_API_URL/v1/runs?job_id=shopify-to-erp&status=failed&limit=20"
```

```json
{
  "schema_version": 1,
  "runs": [
    {
      "id": "run_01HZY7Q1W2E3R4T5Y6U7I8O9P0",
      "job_id": "0195a7c2-4f3b-7d21-9c88-1e2f3a4b5c6d",
      "job_name": "shopify-to-erp",
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
`limit`; `404 not_found` when `job_id` names no job.

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
  "job_id": "shopify-to-erp",
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
  "max_attempts": 3,
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
- `max_attempts` is the retry ceiling the job's manifest currently
  allows, including the first attempt (`retry.attempts: 0` means `1`). It is
  **omitted** when the job is no longer registered, because there is no
  manifest left to resolve it from. A client watching a chain can use it to tell
  a failure that has given up (`latest_status` is `failed` and the newest
  attempt's `attempt` is already at the ceiling) from one whose retry has not
  been recorded yet.
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
  "schema_version": 1,
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

Used by the Python SDK to append runtime log entries from inside a job
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

## HTTP request inspection

A run's outgoing HTTP exchanges are recorded from inside the child and read back
over these endpoints. Capture levels, coverage, redaction, limits and
retention are documented in [http-capture.md](http-capture.md); this section
covers the API only. A per-run level is selected at submission with `?capture=`
on `POST /v1/jobs/{id}/runs`; when it is omitted, the job's
`capture:` field and then the deployment's `--capture-default` decide, and the
shipped default is `full`.

### `POST /v1/runs/{id}/requests/events`

The running child submits a bounded batch of capture events here. Authenticate
with the **run state token** from `OTTER_STATE_TOKEN`, or the daemon token. The
run's job identity is inferred from the run record, never from the
caller, so a token scoped to one run cannot attribute traffic to another.
Delivery is idempotent: a duplicate batch is reported, not double-counted, and a
stale update cannot regress a finalized exchange.

| Parameter | In | Description |
| --- | --- | --- |
| `id` | path | Run id. |
| `body` | JSON body | A capture event batch carrying `schema_version`, `policy`, an optional `adapters` list and a bounded list of `events`. |

The optional `adapters` list names the transport adapters the child actually
installed (`urllib`, `requests`, `httpx`). The daemon merges it into the run's
coverage, so a later partial report can never narrow what was covered. A batch
may carry only an adapter report.

```bash
curl -s -X POST -H "Authorization: Bearer $OTTER_STATE_TOKEN" \
  -H 'Content-Type: application/json' \
  -d @capture-batch.json \
  "$OTTER_API_URL/v1/runs/$OTTER_RUN_ID/requests/events"
```

`202 Accepted`, with a report of what the batch did:

```json
{
  "accepted": 7,
  "duplicates": 1,
  "stale": 0,
  "quota_rejected": 2,
  "dropped_bytes": 4096,
  "finalization": "complete"
}
```

A quota rejection is **not** an error. It appears as `quota_rejected` in this
body because capture is diagnostic: exceeding a limit drops capture rather than
failing a job. A malformed or oversized batch is `400 invalid_request`;
a run with no capture configuration is `409 conflict`. The response never
contains payloads or credentials.

Errors: `400 invalid_request`, `401 unauthorized`, `403 forbidden` (token is not
scoped to this run), `404 not_found`, `409 conflict`.

### `GET /v1/runs/{id}/requests`

List a run's request summaries, oldest first. Operator (admin) authorization:
payload inspection is not part of a run token's narrow scope. The response
carries the run's capture summary alongside the list and never includes
payloads, so a caller can always tell an empty recording from an unavailable
one.

| Query parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `after_id` | integer | `0` | Return only requests with a database `id` greater than this cursor. Must be `>= 0`. |
| `limit` | integer | `100` | Maximum summaries to return; must be a positive integer. |

```bash
curl -s -H "$(auth)" "$OTTER_API_URL/v1/runs/run_01HZY7Q1W2E3R4T5Y6U7I8O9P0/requests?after_id=0&limit=100"
```

```json
{
  "schema_version": 1,
  "capture": {
    "run_id": "run_01HZY7Q1W2E3R4T5Y6U7I8O9P0",
    "state": "complete",
    "policy": "full",
    "adapters": ["urllib", "httpx"],
    "coverage": "urllib, httpx",
    "finalization": "complete",
    "request_count": 2,
    "completed_count": 2,
    "failed_count": 0,
    "incomplete_count": 0,
    "dropped_events": 0,
    "redaction_count": 3,
    "payloads_expired": false
  },
  "requests": [
    {
      "id": 2,
      "request_id": "87603b35e60c4dae9f57040b15e24ab3",
      "phase": "completed",
      "complete": true,
      "method": "POST",
      "url": "http://127.0.0.1:8791/records",
      "status_code": 400,
      "duration_total_ms": 1,
      "occurred_at": "2026-09-22T21:42:17.501585Z",
      "payloads": "full"
    }
  ]
}
```

`state` is the capture state (`unavailable`, `off`, `pending`, `complete`,
`incomplete` or `expired`), which is why the summary travels with the list.
`payloads` is `metadata`, `partial` or `full`; an exchange whose process was
killed is `incomplete`. `adapters` lists the transports the child installed and
`coverage` is the same set rendered for display, so a reader can tell whether an
empty list means "nothing was sent" or "this client was not instrumented".

Errors: `400 invalid_request` for a negative `after_id` or a non-positive
`limit`, `403 forbidden` (a run token), `404 not_found`.

### `GET /v1/runs/{id}/requests/{request_id}`

One exchange, including its sanitized headers and bodies. Operator
authorization: the admin token, or a `capture`-scoped token. A `read` or
`control` credential is refused, because reading the client's bodies is a
separate authority from commanding the runtime. A `request_id` that belongs to a
different run is `404`, not a cross-run read.

| Parameter | In | Description |
| --- | --- | --- |
| `id` | path | Run id. |
| `request_id` | path | The SDK-generated request id from the list. |

```bash
curl -s -H "$(auth)" "$OTTER_API_URL/v1/runs/run_01HZY7Q1W2E3R4T5Y6U7I8O9P0/requests/87603b35e60c4dae9f57040b15e24ab3"
```

```json
{
  "capture": {"run_id": "run_01HZY7Q1W2E3R4T5Y6U7I8O9P0", "state": "complete", "policy": "full"},
  "request": {
    "request_id": "87603b35e60c4dae9f57040b15e24ab3",
    "phase": "completed",
    "complete": true,
    "method": "POST",
    "url": "http://127.0.0.1:8791/records",
    "call_site": "main.py:27 in main",
    "status_code": 400,
    "duration_to_headers_ms": 1,
    "duration_body_ms": 0,
    "duration_total_ms": 1,
    "payloads": "full",
    "request_headers": [
      {"name": "Content-type", "value": "application/json"},
      {"name": "Authorization", "value": "REDACTED"}
    ],
    "request_body": {"state": "captured", "content_type": "application/json", "json": {"cursor": "cur-42"}},
    "response_headers": [{"name": "Content-Type", "value": "application/json"}],
    "response_body": {"state": "captured", "content_type": "application/json", "json": {"error": "cursor rejected"}, "redacted": true, "redacted_count": 1}
  }
}
```

Bodies are always sanitized before storage. A body that v1 cannot capture is
reported as `"state": "omitted"` with a `reason`, never stored raw:
`unsupported_content`, `stream_unsupported`, `oversized`, `incomplete`,
`encoded`, `unparseable`, `redaction_failed`, `quota_exceeded`, `dropped` or
`expired`. An empty body is `"state": "empty"`, which is distinct from an omitted
one.

Errors: `403 forbidden` (a `read` or `control` token, or a run token), `404
not_found` (unknown run, or a `request_id` that belongs to another run).

### `GET /v1/requests/{request_id}`

One exchange addressed by request id alone, with the same body as the run-scoped
read above. The admin token or a `capture`-scoped token. The daemon resolves the
owning run from the stored row; the id does not carry the run, and no prefix or
other client convention is trusted.

Because a request id is only unique within a run, an id that more than one run
recorded is a conflict rather than an arbitrary match. The message names the
candidate runs, so the caller can retry against the run-scoped endpoint.

| Parameter | In | Description |
| --- | --- | --- |
| `request_id` | path | The SDK-generated request id from a run's list. |

```bash
curl -s -H "$(auth)" "$OTTER_API_URL/v1/requests/87603b35e60c4dae9f57040b15e24ab3"
```

The response is identical in shape to `GET /v1/runs/{id}/requests/{request_id}`,
including the `capture` summary for the resolved run.

Errors: `403 forbidden` (a `read` or `control` token, or a run token), `404
not_found` (unknown request id), `409 conflict` (the id is recorded by more than
one run).

### `GET /v1/runs/{id}/timeline`

One chronological page of a **finished** attempt's merged timeline: its lifecycle
lines, captured stdout/stderr and captured HTTP exchanges in one order. It is the
API behind `otter trace`.

Authorization is operator (admin) only, identical to
`GET /v1/runs/{id}/requests`: a per-run token is rejected with `403` even for its
own run, because the merged view exposes captured traffic.

| Query parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `after` | string | unset | An opaque continuation cursor from a previous page's `next_cursor`. Decoded only after authorization, and bounded in length. |
| `limit` | integer | `100` | Maximum **events** per page, excluding context and framing. Must be between `1` and `1000` inclusive. |
| `include_http` | boolean | `true` | When `false`, HTTP exchange events are omitted. The capture summary is still returned, and the context records `include_http: false`. |

```bash
curl -s -H "$(auth)" "$OTTER_API_URL/v1/runs/run_01HZY7Q1W2E3R4T5Y6U7I8O9P0/timeline?limit=4"
```

```json
{
  "context": {
    "schema_version": 1,
    "run_id": "run_01HZY7Q1W2E3R4T5Y6U7I8O9P0",
    "job_id": "int_01HZY5M4N6P7Q8R9S0T1U2V3W4",
    "job_name": "shopify-to-erp",
    "status": "failed",
    "attempt": 1,
    "trigger_type": "manual",
    "error": "process exited with code 1",
    "exit_code": 1,
    "release_digest": "sha256:0f1e2d...",
    "capture_policy": "full",
    "created_at": "2026-01-01T11:59:52Z",
    "started_at": "2026-01-01T11:59:52Z",
    "finished_at": "2026-01-01T11:59:59Z",
    "capture": {"run_id": "run_01HZY7Q1W2E3R4T5Y6U7I8O9P0", "state": "complete", "policy": "full", "finalization": "complete", "request_count": 2},
    "include_http": true
  },
  "events": [
    {"kind": "lifecycle", "at": "2026-01-01T11:59:52Z",     "source": "run_logs",       "id": 1, "run_id": "run_01HZY7Q1W2E3R4T5Y6U7I8O9P0", "stream": "otter",  "message": "run started (attempt 1 of 1, trigger manual)"},
    {"kind": "log",       "at": "2026-01-01T11:59:52.400Z", "source": "run_logs",       "id": 2, "run_id": "run_01HZY7Q1W2E3R4T5Y6U7I8O9P0", "stream": "otter",  "message": "sync starting {\"level\":\"info\",\"object\":\"Contact\"}"},
    {"kind": "http",      "at": "2026-01-01T11:59:53.310Z", "source": "http_exchanges", "id": 1, "run_id": "run_01HZY7Q1W2E3R4T5Y6U7I8O9P0", "http": {"request_id": "87603b35e60c4dae9f57040b15e24ab3", "method": "POST", "url": "http://127.0.0.1:8791/records", "status_code": 400, "duration_total_ms": 1, "phase": "completed", "complete": true, "payloads": "full", "call_site": "main.py:27 in main", "ingested_at": "2026-01-01T11:59:53.412Z", "updated_at": "2026-01-01T11:59:53.415Z"}},
    {"kind": "log",       "at": "2026-01-01T11:59:55.142Z", "source": "run_logs",       "id": 3, "run_id": "run_01HZY7Q1W2E3R4T5Y6U7I8O9P0", "stream": "stderr", "message": "urllib3: retrying request (attempt 1)"},
    {"kind": "log",       "at": "2026-01-01T11:59:58.004Z", "source": "run_logs",       "id": 4, "run_id": "run_01HZY7Q1W2E3R4T5Y6U7I8O9P0", "stream": "stdout", "message": "sync complete: 4242 records"}
  ],
  "has_more": true,
  "next_cursor": "eyJ2IjoxLCJyIjoicnVuXzAxSFpZN1ExVzJF...",
  "snapshot_at": "2026-01-01T12:00:01Z"
}
```

`parent_run_id` is omitted when there is no parent, exactly as `started_at`,
`finished_at` and `exit_code` are omitted when null and `error` when empty. The
grammar of `capture` is the same summary object the request list returns, whose
`state` is `unavailable`, `off`, `pending`, `complete`, `incomplete` or `expired`.

Every event carries `kind`, `at`, `source`, `id` and `run_id`:

| Kind | `source` | Additional fields |
| --- | --- | --- |
| `lifecycle` | `run_logs` | `stream` (`otter`) and `message`, the stored line verbatim. The runtime's own narration about the run. |
| `log` | `run_logs` | `stream` and `message`. Everything the job produced: `stdout`, `stderr`, and its `ctx.log` output. |
| `http` | `http_exchanges` | `http`, the exchange summary. |

The `otter` stream carries **both** `lifecycle` and `log` events. The runtime
narrates a run's lifecycle there, and the SDK's structured logger writes the
job's own `ctx.log` calls to the same stream, so `kind` is decided by who
wrote the line rather than by the stream. A `ctx.log` line is a `log`: it is the
job's output, carrying the fields the job passed.

An `http` object carries `request_id`, `method`, `url` (sanitized), `status_code`
or `transport_error_class`, `duration_total_ms`, `phase`, `complete`, `payloads`,
`call_site`, `ingested_at`, `updated_at`, and `late`, which appears only when
`true`: the daemon recorded the exchange after the producer stamped it, so
adjacent log lines are not evidence of causal order.

`error_code` and `error_message` appear when a failed exchange's response body
held a recognisable reason. They are a bounded, sanitized summary extracted from
that body at ingestion — metadata, not a payload — so a reader learns why a call
was rejected without fetching the exchange. Both are absent when no body was
captured (`capture: metadata`) or when the body carried no error shape.

- Events are ordered by `(at, source_rank, id)`, with `run_logs` ranked below
  `http_exchanges`. Events that share a timestamp therefore order
  deterministically, and a page boundary can neither lose nor repeat one.
- No headers, request or response bodies, trigger payloads or raw run metadata
  ever appear in this response. Payloads stay behind
  `GET /v1/runs/{id}/requests/{request_id}`, deliberately: trigger storage is not
  sanitized to the capture standard, so the merged view does not read it.
- The HTTP event is a summary placed at the exchange's first recorded occurrence;
  its status and duration are the latest retained values and were not necessarily
  known at that timestamp. It is not a response event.
- The ordering is approximate chronology, not causality.
- `snapshot_at` is informational only and does not pin a snapshot; the cursor's
  evidence revision is what guards a continuation.

A run that does not exist is `404`. An attempt that has not finished (queued,
running or retrying) is `409` with a message naming the status. A malformed,
oversized or unknown-version cursor, a cursor for a different run, or one issued
under a different `include_http` setting is `400`.

Continuations are bound to the evidence they started from. If the evidence
changed since the first page — a late log line, an updated exchange, retention
removing payloads, or startup finalization of a pending recording — the
continuation is `409` with a stable message telling the caller to restart without
`after`. A daemon restart alone does not invalidate a cursor.

If the read cannot finish within the daemon's internal budget (250ms, because the
daemon runs SQLite on a single connection that executing runs also write to), the
endpoint returns `503` with code `unavailable` and no partial page. Half a
chronology is worse than a clear failure.

Errors: `400 invalid_request`, `403 forbidden` (a run token), `404 not_found`,
`409 conflict` (the attempt has not finished, or the evidence behind an `after`
cursor changed), `503 unavailable`.

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
jobs and only then terminates them. Use this endpoint when you want a
specific run to stop now, or `otter cancel <run-id>` from the CLI.

Errors: `404 not_found`, `409 conflict` (the run is already `succeeded`,
`failed`, `cancelled` or `timed_out`), `403 forbidden`.

## State

State is a per-job JSON key/value store, durable in SQLite, readable and
writable both from Python (`ctx.state`) and over HTTP. Values are arbitrary
JSON. Keys must match `[A-Za-z0-9._:-]{1,128}`.

The SDK uses a run state token for these endpoints; operators use the daemon
token. Both are accepted, and the daemon token can address any job.

### `GET /v1/jobs/{id}/state`

Return the whole state map.

```bash
curl -s -H "$(auth)" "$OTTER_API_URL/v1/jobs/shopify-to-erp/state"
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
values all round-trip unchanged. A job with no state returns
`{"state": {}}`.

Errors: `404 not_found`, `403 forbidden` (a run state token for another
job).

### `GET /v1/jobs/{id}/state/{key}`

Read one key. **The response body is the raw JSON value with no envelope**, which
is what makes `otter state get <job> <key>` printable verbatim:

```bash
curl -s -H "$(auth)" "$OTTER_API_URL/v1/jobs/shopify-to-erp/state/cursor"
```

```json
"123"
```

A number round-trips as a number (`12`), an object as an object
(`{"runs":17}`); there is no double encoding. The `Content-Type` is
`application/json; charset=utf-8`.

Errors: `404 not_found` (job or key missing), `400 invalid_request` (the
key does not match `[A-Za-z0-9._:-]{1,128}`), `403 forbidden`.

### `PUT /v1/jobs/{id}/state/{key}`

Create or replace one key. This is an upsert, so it works for new and existing
keys.

```bash
curl -s -X PUT -H "$(auth)" -H 'Content-Type: application/json' \
  -d '"123"' "$OTTER_API_URL/v1/jobs/shopify-to-erp/state/cursor"
```

Request body: any JSON value. To store the string `123` send `"123"` (with
quotes); to store the number 123 send `123`.

```json
{
  "job_id": "shopify-to-erp",
  "key": "cursor",
  "value": "123",
  "updated_at": "2024-06-01T12:40:11Z"
}
```

Errors: `400 invalid_request` (empty or invalid JSON body, or an invalid key),
`404 not_found`, `403 forbidden`.

### `DELETE /v1/jobs/{id}/state/{key}`

Delete one key.

```bash
curl -s -X DELETE -H "$(auth)" \
  "$OTTER_API_URL/v1/jobs/shopify-to-erp/state/cursor"
```

```json
{
  "job_id": "shopify-to-erp",
  "key": "cursor",
  "deleted": true
}
```

Unlike a PUT, deleting a key that is not set returns `404`, so a cleanup script
that must be idempotent should treat `404` as success.

Errors: `400 invalid_request` (invalid key), `404 not_found`, `403 forbidden`.

## Webhooks

### `POST /v1/hooks/{job}`

Enqueue a run from an external system. Requires the job's webhook token
(`X-Otter-Token` header preferred, `?token=` accepted), **not** the daemon
token. Available only when `trigger.webhook.enabled: true` in the manifest; the
endpoint returns `404` otherwise, so disabled hooks are indistinguishable from
nonexistent ones.

| Parameter | In | Description |
| --- | --- | --- |
| `job` | path | Job `name`. |
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

A hook does not run the job inline and does not wait for it: it enqueues
a run and returns immediately, so a slow job cannot make a webhook caller
time out. The request headers are recorded on the run for `ctx.trigger.headers`,
**except** `Authorization` and `X-Otter-Token`, which are never persisted.

Errors:

| Status | `code` | Cause |
| --- | --- | --- |
| `401` | `unauthorized` | Missing or wrong `X-Otter-Token` / `token`. |
| `404` | `not_found` | Unknown job, or its webhook trigger is disabled (deliberately identical, so the endpoint cannot enumerate jobs). |
| `503` | `unavailable` | The job is paused. The caller already holds a valid token, so naming the pause leaks nothing; `404` here would look like a configuration error. Resume with `otter resume` to accept triggers again. |
| `403` | `forbidden` | A daemon or run token was used on the hook route. |
| `400` | `invalid_request` | Empty job path segment. |
| `500` | `internal_error` | The run could not be enqueued. |

## End-to-end curl walkthrough

```bash
export OTTER_API_URL=http://127.0.0.1:7337
auth() { [ -n "$OTTER_API_TOKEN" ] && printf 'Authorization: Bearer %s' "$OTTER_API_TOKEN"; }

# 1. Is it alive?
curl -s "$OTTER_API_URL/health"

# 2. What jobs exist?
curl -s -H "$(auth)" "$OTTER_API_URL/v1/jobs" | python3 -m json.tool

# 3. Start one manually and capture the run id.
RUN_ID=$(curl -s -X POST -H "$(auth)" -H 'Content-Type: application/json' -d '{}' \
  "$OTTER_API_URL/v1/jobs/shopify-to-erp/runs" \
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
curl -s -H "$(auth)" "$OTTER_API_URL/v1/jobs/shopify-to-erp/state"
curl -s -X PUT -H "$(auth)" -H 'Content-Type: application/json' \
  -d '"2024-06-01T00:00:00Z"' \
  "$OTTER_API_URL/v1/jobs/shopify-to-erp/state/backfill_after"

# 7. Cancel it if it is still going.
curl -s -X POST -H "$(auth)" "$OTTER_API_URL/v1/runs/$RUN_ID/cancel"

# 8. Fire a webhook.
TOKEN=$(curl -s -H "$(auth)" "$OTTER_API_URL/v1/jobs/order-events" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["webhook_token"])')
curl -s -X POST -H "X-Otter-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"order_id": 4242}' "$OTTER_API_URL/v1/hooks/order-events"

# 9. After a failure, read its merged timeline — lifecycle, output and HTTP
#    exchanges in one order. This is the API behind `otter trace`.
curl -s -H "$(auth)" "$OTTER_API_URL/v1/runs/$RUN_ID/timeline?limit=200" \
  | python3 -m json.tool
```

The same actions through the CLI:

```bash
otter status
otter jobs
otter inspect shopify-to-erp
otter run shopify-to-erp
otter runs shopify-to-erp --status failed --limit 20
otter run-status <run-id>
otter logs <run-id> --follow
otter state get shopify-to-erp cursor
otter state set shopify-to-erp cursor '"123"'
otter state delete shopify-to-erp cursor
otter validate my-job
```

Add `--json` to any CLI command for raw JSON instead of the human-readable
default, and `--api`/`--token` to target a different daemon:

```bash
otter --api https://otter.internal.example.com --token "$OTTER_API_TOKEN" \
  --json runs --status failed --limit 5
```
