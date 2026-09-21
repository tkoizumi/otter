# Manifest Reference (`otter.yaml`)

Every integration directory contains exactly one file named `otter.yaml`. The
daemon finds integrations by walking the root passed to `--integrations` and
looking for that filename. Nothing else is required to register an integration.

Discovery skips directories that cannot contain integrations and are expensive
to walk: `.git`, `.hg`, `.svn`, `.cache`, `.venv`, `venv`, `node_modules`,
`__pycache__`, `.mypy_cache`, `.pytest_cache`, `.tox`, `dist` and `build`.

- [Minimal manifest](#minimal-manifest)
- [Full manifest](#full-manifest)
- [Field reference](#field-reference)
- [Triggers](#triggers)
- [Retries](#retries)
- [Timeouts and concurrency](#timeouts-and-concurrency)
- [Environment variables](#environment-variables)
- [Secrets](#secrets)
- [Naming and uniqueness](#naming-and-uniqueness)
- [Entrypoint rules](#entrypoint-rules)
- [Validation and invalid integrations](#validation-and-invalid-integrations)
- [Examples](#examples)

## Minimal manifest

```yaml
version: 1
name: example
entrypoint: main.py
```

That is a valid, runnable integration: no trigger (manual runs only), the
default 300-second timeout, concurrency 1, and no retries.

## Full manifest

```yaml
version: 1

name: shopify-to-erp
description: Sync Shopify orders into ERP.
entrypoint: main.py

python:
  executable: python3

trigger:
  cron: "*/5 * * * *"
  webhook:
    enabled: true

timeout: 300
concurrency: 1

retry:
  attempts: 5
  backoff: exponential
  initial_delay: 2s
  max_delay: 60s

env:
  SHOPIFY_STORE: example.myshopify.com

secrets:
  - SHOPIFY_TOKEN
  - ERP_TOKEN
```

## Field reference

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `version` | integer | **yes** | — | Manifest schema version. Must be `1`. |
| `name` | string | **yes** | — | Human-facing label, used by the CLI and shown in the API. Must match `^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`. Labels need not be unique; the durable identity is separate. See [identity.md](identity.md). |
| `description` | string | no | `""` | Free-form human description, returned by the API and shown by `otter inspect`. |
| `entrypoint` | string | **yes** | — | Path to the Python file to run, relative to the integration directory. Must stay inside the directory and must exist. |
| `python.mode` | string | no | `external` | `external` preserves host Python behavior. `managed` requires `.python-version`, `pyproject.toml`, and `uv.lock`; prepare it before running. |
| `python.executable` | string | no | `python3` in external mode | Interpreter used to launch the entrypoint. Resolved on `PATH` or given as an absolute path. Cannot be set in managed mode. |
| `python.path` | list of strings | no | `[]` | Extra directories prepended to the child's `PYTHONPATH`. The declared directories are captured into the integration's release at the same relative depth, so the same relative paths keep working after activation. |
| `trigger.cron` | string | no | unset | Standard 5-field cron expression (`minute hour day-of-month month day-of-week`). Omit for no schedule. |
| `trigger.webhook.enabled` | boolean | no | `false` | When `true`, exposes `POST /v1/hooks/{name}` guarded by a per-integration token. |
| `timeout` | integer \| string | no | `300` | Maximum wall-clock time for one attempt. An integer means seconds; a string is a Go duration (`30s`, `5m`, `1h30m`). |
| `concurrency` | integer | no | `1` | Maximum number of simultaneous runs of **this** integration. Extra triggers queue. Must be `>= 1`. |
| `retry.attempts` | integer | no | `0` | **Total** number of attempts, including the first. Must be `>= 0`. |
| `retry.backoff` | string | no | `exponential` | One of `none`, `linear`, `exponential`. |
| `retry.initial_delay` | string | no | `2s` | Delay before the second attempt. Go duration string. |
| `retry.max_delay` | string | no | `60s` | Upper bound on any single backoff delay. Go duration string. |
| `env` | map[string]string | no | `{}` | Extra environment variables for the child process. Values may reference daemon environment variables with `${VAR}`. |
| `secrets` | []string | no | `[]` | Names of environment variables read from the **daemon's** environment and injected into the child process. |

Unknown fields are rejected rather than ignored, so a typo such as `timeouts:`
fails validation immediately instead of silently taking the default.

Additional limits enforced at validation time:

| Field | Constraint |
| --- | --- |
| `name` | At most 64 characters. |
| `retry.attempts` | At most 100. |
| `timeout` | At most 30 days (`720h`). |
| `retry.max_delay` | Must be greater than or equal to `retry.initial_delay`. |
| `retry.initial_delay`, `retry.max_delay` | Must not be negative. |
| `entrypoint` | Must be a file, not a directory. |
| `env` keys, `secrets` entries | Must be valid environment variable names; `secrets` entries must be unique and non-empty. |

Validation reports **every** problem in a manifest at once rather than stopping
at the first one.

## Triggers

An integration can have a cron trigger, a webhook trigger, both, or neither.
Manual runs (`otter run <name>`, `POST /v1/integrations/{id}/runs`) work for
every integration regardless of its trigger configuration.

### Cron

```yaml
trigger:
  cron: "*/5 * * * *"
```

- Standard **5-field** cron: `minute hour day-of-month month day-of-week`. There
  is no seconds field. The usual descriptors (`@hourly`, `@daily`, `@weekly`,
  `@monthly`, `@yearly`, `@every 5m`) are accepted too.
- Schedules are reconciled from manifests on every daemon start and on every
  `otter reload`. Editing a cron expression and reloading is the way to change a
  schedule — the daemon does not need to be restarted, and integrations whose
  expression did not change keep their next fire time.
- **Missed occurrences are not replayed.** If the daemon is offline from 12:00
  to 12:20 with a `*/5` schedule, the four missed ticks are gone. Design
  integrations to reconcile from a checkpoint stored in `ctx.state` instead of
  assuming one run per tick.

Useful expressions:

| Expression | Meaning |
| --- | --- |
| `* * * * *` | Every minute. |
| `*/5 * * * *` | Every 5 minutes. |
| `0 * * * *` | Top of every hour. |
| `0 3 * * *` | Daily at 03:00. |
| `30 6 * * 1` | Mondays at 06:30. |

### Webhook

```yaml
trigger:
  webhook:
    enabled: true
```

Exposes `POST /v1/hooks/{name}`. On first start the daemon generates a static
token for the integration, stores it in SQLite, and returns it from
`GET /v1/integrations/{id}`. The caller must present it as `X-Otter-Token:
<token>` or `?token=<token>`. The request body and headers are handed to Python
through `ctx.trigger`.

```bash
TOKEN=$(curl -s -H "Authorization: Bearer $OTTER_API_TOKEN" \
  http://127.0.0.1:7337/v1/integrations/my-hook | python3 -c 'import json,sys;print(json.load(sys.stdin)["webhook_token"])')

curl -s -X POST -H "X-Otter-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"order_id": 4242}' http://127.0.0.1:7337/v1/hooks/my-hook
```

### Manual only

Omit `trigger` entirely (or leave both sub-fields unset). The integration can
only be started by `otter run <name>` or the API.

## Retries

```yaml
retry:
  attempts: 5
  backoff: exponential
  initial_delay: 2s
  max_delay: 60s
```

- `attempts` is the **total** number of attempts. `attempts: 5` means one initial
  attempt plus up to four retries. `attempts: 0` (the default) means a single
  attempt and no retry at all.
- `backoff` controls how the delay grows between attempts:

  | Value | Delay before attempt *n* (n ≥ 2) |
  | --- | --- |
  | `none` | No delay — the retry is enqueued immediately. |
  | `linear` | `initial_delay * (n - 1)`. |
  | `exponential` | `initial_delay * 2^(n - 2)`. |

- Every delay is capped at `max_delay`. There is deliberately **no jitter**, so
  the delays are exactly what the manifest says.

With `initial_delay: 2s`, `max_delay: 60s`, `attempts: 5` and exponential
backoff, the delays are **2s, 4s, 8s, 16s** — five attempts in total.

Linear backoff with `initial_delay: 5s`, `attempts: 4` waits **5s, 10s, 15s**.

A retry is a **new run record**. The new run has `attempt` incremented,
`parent_run_id` pointing at the previous attempt, and the same `root_run_id`.
While an attempt is waiting out its backoff it is visible as `retrying`.

### What is retried, and what is not

| Outcome | Retried? |
| --- | --- |
| Non-zero process exit | **Yes** |
| Timeout (`timed_out`) | **Yes** |
| `failed` after crash recovery (`otter daemon restarted during execution`) | **Yes** |
| `failed` after shutdown (`otter daemon shut down during execution`) | **Yes** |
| Cancelled run (`POST /v1/runs/{id}/cancel`, Ctrl-C) | No |
| Missing or non-executable entrypoint | No |
| Invalid manifest | No |
| Secret listed in `secrets` but absent from the daemon environment | No |

The last four are configuration failures: retrying them would repeat an
identical mistake. A missing secret fails the run **before Python starts**, and
the error is recorded on the run and in its `otter`-stream logs.

## Timeouts and concurrency

```yaml
timeout: 300        # 300 seconds
timeout: 30s        # the same thing, duration string
timeout: 5m         # 300 seconds
concurrency: 2
```

On timeout the daemon sends `SIGTERM` to the child's process group, waits about
5 seconds, then `SIGKILL`s it. The run is marked `timed_out` and the retry
policy applies.

`concurrency` limits simultaneous runs **of this integration**; extra triggers
stay queued. The global `--workers` flag caps total concurrent runs across all
integrations (default: number of CPU cores, capped at 8). Both limits apply at
once — a manifest `concurrency: 8` on a machine started with `--workers 2` still
runs at most two integrations at a time.

## Environment variables

```yaml
env:
  SHOPIFY_STORE: example.myshopify.com
  API_BASE: "https://${REGION}.internal.example.com"
  LOG_LEVEL: debug
```

- Values are injected into the child process.
- `${VAR}` is expanded **at run time** against the daemon's environment; an
  unset variable expands to the empty string. Plain `$VAR` also works, but
  `${VAR}` is preferred because it cannot run into the next character.
- Use `env` for configuration, `secrets` for credentials.

The runtime always adds these variables to the child environment, which override
anything an integration attempts to set with the same names:

`OTTER_INTEGRATION_ID`, `OTTER_INTEGRATION_NAME`, `OTTER_RUN_ID`, `OTTER_API_URL`,
`OTTER_STATE_TOKEN`, `OTTER_TRIGGER_TYPE`, `OTTER_INTEGRATION_DIR`.

## Secrets

```yaml
secrets:
  - SHOPIFY_TOKEN
  - ERP_TOKEN
```

Secrets are read from the **daemon's** environment, not from the manifest and not
from a `.env` file in the integration directory. The names listed are copied into
the child process's environment just before execution.

- Keep values out of YAML: put `SHOPIFY_TOKEN=...` in the systemd
  `EnvironmentFile` (mode `0600`, owned by the service user) or a secret manager
  that populates the daemon environment.
- If a listed name is missing from the daemon environment, the run **fails
  before Python starts** with an error naming the missing secret, and it is not
  retried. A variable that is set but empty counts as missing — a blank token is
  a misconfiguration, not a secret.
- The daemon never logs secret values. Names may appear in error messages.
- Today there is no built-in secret backend; the `SecretProvider` interface in
  the codebase is the extension point for AWS Secrets Manager, Vault, 1Password
  or Castor Cloud later. See [security.md](security.md).

## Naming and uniqueness

- `name` must match `^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`: lowercase letters,
  digits, and `.`, `_`, `-` in the middle; must start and end with a letter or
  digit. `shopify-to-erp` and `erp.sync_v2` are valid; `Shopify-Sync`, `_x` and
  `-x-` are not.
- `name` is a **label**, not a key. State, run history, webhook tokens, releases
  and prepared environments belong to the integration's durable **identity**,
  which the runtime mints once and records in a `.otter-id` marker inside the
  directory. See [identity.md](identity.md).
- Labels do **not** have to be unique. Two integrations may declare the same
  `name`; `otter run <name>` then refuses and lists every candidate, and either
  `id:<id>` or a filesystem path selects one unambiguously.
- A reference is a label, a path, or `id:<id>`. `otter run .` and a bare
  `otter run` read the working directory's `otter.yaml`, and `otter inspect .`,
  `otter release` and `otter prepare` do the same. A path is resolved through
  the registry's path ownership, never by matching a manifest name.
- Editing `name` preserves the identity, and therefore the state and history. The
  new label is recorded on the next scan.
- The directory name is not identity either. Copying a directory creates a new
  instance with a fresh identity and empty state; renaming one in place, and
  recording it with `otter move`, keeps the identity.

## Entrypoint rules

- `entrypoint` is relative to the integration directory.
- It must resolve **inside** that directory. `../shared/main.py` and absolute
  paths are rejected.
- The file must exist at validation time; a missing entrypoint makes the
  integration invalid and is never a retryable failure.
- The child process runs with the integration directory as its working
  directory, so relative paths inside `main.py` resolve naturally next to the
  manifest.
- In external mode, Otter does not install `requirements.txt`; provision the
  interpreter yourself. In managed mode, pin an exact CPython patch version in
  `.python-version`, declare dependencies in `pyproject.toml`, commit `uv.lock`,
  and run `otter prepare --integrations <root> --data <data-dir>` before starting
  the daemon. Preparation requires `uv` on the target host; pass `--uv <path>`
  to select it. Managed runs use only a prepared environment and never fall back
  to host Python.

## Validation and invalid integrations

Validate without starting the daemon:

```bash
otter validate ./my-integration
otter validate ./my-integration/otter.yaml
otter validate my-integration               # by label, from inside the workspace
```

`otter validate` exits non-zero and prints the field-level error on failure.

At runtime, an invalid integration **never** crashes the daemon:

- the error is written to the daemon log (`"event":"integration_invalid"`),
- the integration appears in the API (`GET /v1/integrations`) with
  `"valid": false` and an `"error"` string,
- it is hidden from `otter integrations` unless you pass `--all`,
- it cannot be run until the manifest is fixed and the daemon reloads it.

## Examples

### Cron only

```yaml
version: 1
name: hourly-report
entrypoint: main.py

trigger:
  cron: "0 * * * *"

timeout: 10m
retry:
  attempts: 3
  backoff: exponential
  initial_delay: 30s
  max_delay: 5m
```

### Webhook only

```yaml
version: 1
name: github-deploy-hook
entrypoint: main.py

trigger:
  webhook:
    enabled: true

timeout: 60
concurrency: 4
```

### Manual only

```yaml
version: 1
name: backfill-orders
entrypoint: main.py

timeout: 30m

env:
  BACKFILL_MODE: "1"
```

### Retries with linear backoff

```yaml
version: 1
name: flaky-partner-api
entrypoint: main.py

trigger:
  cron: "*/15 * * * *"

retry:
  attempts: 4            # 1 initial attempt + up to 3 retries
  backoff: linear
  initial_delay: 5s      # waits 5s, 10s, 15s
  max_delay: 2m
```

### Secrets and environment together

```yaml
version: 1
name: shopify-to-erp
entrypoint: main.py

python:
  executable: /opt/otter/venv/bin/python3
  # Shared client code, so several integrations can import one Shopify or
  # ERP client instead of each carrying its own copy.
  path:
    - ../../lib/python

trigger:
  cron: "*/5 * * * *"

env:
  SHOPIFY_STORE: example.myshopify.com
  API_BASE: "https://${REGION}.internal.example.com"

secrets:
  - SHOPIFY_TOKEN
  - ERP_TOKEN
```

The resulting `PYTHONPATH` is ordered deliberately: the runtime SDK first (so
`import otter` always resolves to the daemon's own copy), then `python.path`
entries, then whatever the daemon inherited. An operator-supplied `PYTHONPATH`
is therefore preserved rather than replaced.

### High-throughput webhook with retries

```yaml
version: 1
name: order-events
entrypoint: main.py

trigger:
  webhook:
    enabled: true

timeout: 120
concurrency: 8

retry:
  attempts: 5
  backoff: exponential
  initial_delay: 1s
  max_delay: 30s
```

### No retries, fail fast

```yaml
version: 1
name: destructive-migration
entrypoint: main.py

timeout: 2h
retry:
  attempts: 0     # the default: exactly one attempt, no retry
```
