# Examples

Two examples ship in `examples/` so that `otter start --integrations ./examples` starts something
meaningful. This document covers both, then walks through the manifest patterns
that are **not** shipped as directories — keep-alive originals you can copy into
your own integrations root.

- [Shipped: `examples/counter`](#shipped-examplescounter)
- [Shipped: `examples/customer-sync`](#shipped-examplescustomer-sync)
- [Pattern: hello world (manual only)](#pattern-hello-world-manual-only)
- [Pattern: cron-scheduled sync](#pattern-cron-scheduled-sync)
- [Pattern: webhook-triggered integration](#pattern-webhook-triggered-integration)
- [Notes on the shipped examples](#notes-on-the-shipped-examples)

## Shipped: `examples/counter`

[`examples/counter`](../examples/counter) is the smallest useful integration: a
few lines of `main.py` that increment a durable counter every minute. It is the
fixture for demonstrating scheduling, state, logs and run history in one place.

```text
examples/counter/
├── otter.yaml
└── main.py
```

```yaml
# examples/counter/otter.yaml
version: 1

name: counter

entrypoint: main.py

trigger:
  cron: "0 * * * *"

retry:
  attempts: 3

timeout: 30
```

Nothing else is configured, which is the point: `concurrency` defaults to `1`,
the timeout is 30 seconds, and the retry block uses the documented defaults for
`backoff` (`exponential`), `initial_delay` (`2s`) and `max_delay` (`60s`) —
three total attempts, so a failing counter run is retried up to twice.

```python
# examples/counter/main.py
from otter import Context

ctx = Context.from_environment()

count = ctx.state.get("count") or 0
count += 1

ctx.log.info("Counter executed", count=count)

ctx.state.set("count", count)
```

This example uses the explicit `Context.from_environment()` style (rather than
the `@run` decorator shown in
[hello world](#pattern-hello-world-manual-only)) so both SDK entry points appear
in the shipped code.

What it demonstrates:

- **Cron scheduling.** `"0 * * * *"` fires hourly. `otter inspect counter` shows the
  registered expression and the next fire time, and a new `cron`-triggered run
  appears in `otter runs` without touching the daemon. The example uses an hourly
  schedule rather than `* * * * *` so that the README walkthrough, where three
  manual runs must read `1`, `2` and `3`, cannot be raced by a scheduled run.
- **Durable state.** `count` survives restarts, reboots and the process boundary:
  it lives in the `integration_state` table, not in Python memory.
- **Logs.** `ctx.log.info(...)` writes a structured line into `run_logs` with the
  `otter` stream; the keyword arguments become fields on the log entry.
- **Run history.** Every execution is a row in `runs`, visible through
  `otter runs` and `otter run-status`.
- **Default concurrency.** Counter increments would race at higher concurrency,
  so the example relies on the default `concurrency: 1`.

## Shipped: `examples/customer-sync`

[`examples/customer-sync`](../examples/customer-sync) is the realistic pattern: a
mock source API, a transform, and a mock destination API, checkpointed so that a
crash resumes where it left off.

```text
examples/customer-sync/
├── otter.yaml
├── main.py
└── mock_api.py
```

The integration talks to a single local mock server that plays **both** roles —
a source endpoint and a destination endpoint on `http://127.0.0.1:8899` — so the
example runs offline and deterministically. Its checkpoint key is
`last_processed_customer_id`, written to `ctx.state` after every customer (not
after every page), so a crash mid-page resumes at the next customer.

```yaml
# examples/customer-sync/otter.yaml
version: 1

name: customer-sync

description: Sync customers from a mock source API into a mock destination API.

entrypoint: main.py

python:
  executable: python3

timeout: 120

concurrency: 1

retry:
  attempts: 3
  backoff: exponential
  initial_delay: 1s
  max_delay: 10s

env:
  SOURCE_API_URL: http://127.0.0.1:8899
  DEST_API_URL: http://127.0.0.1:8899
```

Note that this example has **no trigger block**: it is triggered manually with
`otter run customer-sync`, which is the fastest way to iterate on it.

### Running it

Start the mock API first — the integration has no data to sync without it:

```bash
python3 examples/customer-sync/mock_api.py
```

In a second terminal, validate and run the integration:

```bash
otter validate examples/customer-sync
RUN_ID=$(otter run customer-sync)
echo "$RUN_ID"

otter run-status "$RUN_ID"
otter logs "$RUN_ID"
otter state get customer-sync last_processed_customer_id
```

Expected output:

```console
$ python3 examples/customer-sync/mock_api.py
mock source+destination API listening on http://127.0.0.1:8899
  source:      GET  http://127.0.0.1:8899/source/customers?after=0&limit=5
  destination: POST http://127.0.0.1:8899/dest/customers
  destination: GET  http://127.0.0.1:8899/dest/customers
  health:      GET  http://127.0.0.1:8899/health
```

```console
$ otter validate examples/customer-sync
ok: customer-sync (/home/me/otter/examples/customer-sync/otter.yaml)

1 integration(s) valid

$ otter run customer-sync
run_01HZY7Q1W2E3R4T5Y6U7I8O9P0
```

```console
$ otter run-status run_01HZY7Q1W2E3R4T5Y6U7I8O9P0
run id:        run_01HZY7Q1W2E3R4T5Y6U7I8O9P0
integration:   customer-sync
trigger:       manual
status:        succeeded
attempt:       1
created:       2024-06-01T12:34:56Z
started:       2024-06-01T12:34:56Z
finished:      2024-06-01T12:34:57Z
duration:      812ms
exit code:     0
```

`otter --json run-status <run-id>` shows the same record as JSON, with the full
retry chain in `attempts`.

The 25 customers are delivered five per page. Each one is logged as it is
checkpointed, so a successful run ends with:

```console
$ otter logs run_01HZY7Q1W2E3R4T5Y6U7I8O9P0
... (one line per customer, oldest first)
{"level":"info","event":"customer synced","customer_id":24,"name":"...","checkpoint":24}
{"level":"info","event":"customer synced","customer_id":25,"name":"...","checkpoint":25}
synced 25 customers; checkpoint=25
{"level":"info","event":"customer sync finished","synced":25,"checkpoint":25}
```

```console
$ otter state get customer-sync last_processed_customer_id
25
```

Running it again processes nothing new, which is the point of the checkpoint:

```console
$ otter run customer-sync
run_01HZY7Q1W2E3R4T5Y6U7I8O9P1

$ otter logs run_01HZY7Q1W2E3R4T5Y6U7I8O9P1
{"level":"info","event":"customer sync starting","checkpoint":25,...}
synced 0 customers; checkpoint=25
{"level":"info","event":"customer sync finished","synced":0,"checkpoint":25}
```

### Simulating a crash and resuming

`CRASH_AFTER=n` makes the integration raise an error after `n` customers have
been delivered and checkpointed in the current run. Because the checkpoint is
written per customer, the retry (and any later manual run) resumes at customer
`n + 1` instead of starting over.

`CRASH_AFTER` is read from the environment of the **daemon**, which is where the
child process inherits it from. Restart the daemon with it set:

```bash
# Crash after 3 customers:
CRASH_AFTER=3 otterd --integrations ./examples --data ./tmp --log-format pretty
```

Expected — the first attempt fails, and because the manifest retries (up to
three attempts) the retry resumes from the checkpoint:

```console
$ otter run customer-sync
run_01HZY8A1B2C3D4E5F6G7H8I9J0

$ otter run-status run_01HZY8A1B2C3D4E5F6G7H8I9J0
run id:        run_01HZY8A1B2C3D4E5F6G7H8I9J0
integration:   customer-sync
trigger:       manual
status:        failed
attempt:       1
created:       2024-06-01T12:36:00Z
started:       2024-06-01T12:36:00Z
finished:      2024-06-01T12:36:00Z
duration:      41ms
exit code:     1
error:         simulated crash after 3 customers

$ otter state get customer-sync last_processed_customer_id
3
```

```console
$ otter logs run_01HZY8A1B2C3D4E5F6G7H8I9J0
{"level":"info","event":"customer sync starting","checkpoint":0,...,"crash_after":3}
{"level":"info","event":"customer synced","customer_id":1,...}
{"level":"info","event":"customer synced","customer_id":2,...}
{"level":"info","event":"customer synced","customer_id":3,...}
Traceback (most recent call last):
  ...
RuntimeError: simulated crash after 3 customers
```

Each retry runs in a fresh process, so `CRASH_AFTER=3` makes it fail at customer
6 on the second attempt and at customer 9 on the third. With the policy
exhausted, the run ends as `failed` and the checkpoint sits at 9:

```console
$ otter runs --integration customer-sync --limit 5
RUN ID                        INTEGRATION    TRIGGER  STATUS  ATTEMPT  STARTED               DURATION
run_01HZY8A1B2C3D4E5F6G7H8I9J2  customer-sync  manual   failed  3        2024-06-01T12:36:02Z  38ms
run_01HZY8A1B2C3D4E5F6G7H8I9J1  customer-sync  manual   failed  2        2024-06-01T12:36:01Z  40ms
run_01HZY8A1B2C3D4E5F6G7H8I9J0  customer-sync  manual   failed  1        2024-06-01T12:36:00Z  41ms
```

Unset `CRASH_AFTER` (restart the daemon without it) and the next manual run
completes the remaining 16 customers. To replay from the beginning:

```bash
otter state delete customer-sync last_processed_customer_id
```

### Demonstrating retries

`MOCK_FAIL_AFTER=n` makes the mock **destination** start returning HTTP 500
after it has accepted `n` customers. Restart the mock with it set — this is an
environment variable of the mock process, not of the daemon, so only the mock
needs restarting:

```bash
MOCK_FAIL_AFTER=5 python3 examples/customer-sync/mock_api.py
```

```console
$ MOCK_FAIL_AFTER=5 python3 examples/customer-sync/mock_api.py
mock source+destination API listening on http://127.0.0.1:8899
  ...
simulated failure: POST /dest/customers fails after 5 accepted customers
```

Now a run delivers five customers, fails on the sixth, and Otter retries with
exponential backoff (`initial_delay: 1s`, doubling, capped at `max_delay: 10s`):

```console
$ otter run customer-sync
run_01HZY9B2C3D4E5F6G7H8I9J0K1

$ otter run-status run_01HZY9B2C3D4E5F6G7H8I9J0K1
run id:        run_01HZY9B2C3D4E5F6G7H8I9J0K1
integration:   customer-sync
trigger:       manual
status:        retrying
attempt:       1
created:       2024-06-01T12:40:00Z
started:       2024-06-01T12:40:00Z
finished:      2024-06-01T12:40:00Z
duration:      63ms
exit code:     1
error:         destination API rejected customer 6 with HTTP 500: {'error': 'simulated destination failure'}
```

While the backoff is pending, the *next* attempt already exists as its own run
with status `retrying`; `otter run-status` on that id reports the chain:

```console
$ otter run-status run_01HZY9B2C3D4E5F6G7H8I9J0K2
run id:        run_01HZY9B2C3D4E5F6G7H8I9J0K2
integration:   customer-sync
trigger:       manual
status:        retrying
attempt:       2
retry of:      run_01HZY9B2C3D4E5F6G7H8I9J0K1
root run:      run_01HZY9B2C3D4E5F6G7H8I9J0K1
latest status: retrying
created:       2024-06-01T12:40:00Z
```

```console
$ otter runs --integration customer-sync --limit 5
RUN ID                        INTEGRATION    TRIGGER  STATUS    ATTEMPT  STARTED               DURATION
run_01HZY9B2C3D4E5F6G7H8I9J0K2  customer-sync  manual   retrying  2        -                     0s
run_01HZY9B2C3D4E5F6G7H8I9J0K1  customer-sync  manual   failed    1        2024-06-01T12:40:00Z  63ms
```

The checkpoint has already advanced past the five accepted customers, so every
retry picks up at customer 6 rather than re-delivering customers 1–5:

```console
$ otter state get customer-sync last_processed_customer_id
5
```

Because the mock keeps failing, all three attempts fail and the run ends as
`failed`. Stop the mock, restart it without `MOCK_FAIL_AFTER`, and run again:

```bash
otter run customer-sync          # delivers customers 6..25
otter state get customer-sync last_processed_customer_id
# 25
```

Inspect the whole chain at once:

```bash
otter --json run-status run_01HZY9B2C3D4E5F6G7H8I9J0K1 | python3 -m json.tool
```

Both environment variables are read by the example code from the process
environment the daemon passes down, so you can experiment without editing the
manifest:

| Variable | Read by | Effect |
| --- | --- | --- |
| `CRASH_AFTER=n` | `main.py` (via the daemon's environment) | Raise after `n` customers in the current run, simulating a crash mid-sync. |
| `MOCK_FAIL_AFTER=n` | `mock_api.py` | Destination returns HTTP 500 after `n` accepted customers, exercising retries. |
| `MOCK_PORT`, `MOCK_HOST` | `mock_api.py` | Bind the mock somewhere else (default `127.0.0.1:8899`). |
| `SOURCE_API_URL`, `DEST_API_URL` | `main.py` (set by the manifest) | Point the integration at different API base URLs. |

## Pattern: hello world (manual only)

The smallest possible integration: no trigger, one command to run it.

```text
integrations/hello-world/
├── otter.yaml
└── main.py
```

```yaml
# integrations/hello-world/otter.yaml
version: 1

name: hello-world
description: Print a greeting and remember how many times it has run.

entrypoint: main.py

timeout: 30
retry:
  attempts: 0          # the default: exactly one attempt, no retries
```

```python
# integrations/hello-world/main.py
import os

from otter import Context

ctx = Context.from_environment()

previous = ctx.state.get("greeted", 0)
ctx.state.set("greeted", previous + 1)

ctx.log.info("hello from Otter", integration=ctx.integration_id, trigger=ctx.trigger.type)
print(f"hello world (run #{previous + 1})")
print(f"integration dir: {os.environ['OTTER_INTEGRATION_DIR']}")
```

Run it:

```bash
otterd --integrations ./integrations --data ./tmp --log-format pretty &
otter validate ./integrations/hello-world
otter run hello-world
```

Expected:

```console
$ otter validate ./integrations/hello-world
ok: hello-world (/home/me/integrations/hello-world/otter.yaml)

1 integration(s) valid

$ otter run hello-world
run_01HZYA1B2C3D4E5F6G7H8I9J0K1

$ otter logs run_01HZYA1B2C3D4E5F6G7H8I9J0K1
run started (attempt 1 of 1, trigger manual)
hello world (run #1)
integration dir: /home/me/integrations/hello-world
run succeeded (attempt 1, 45ms), exit code 0

$ otter state get hello-world greeted
1
```

Run it twice more and `greeted` is `3` — state persists across runs because it
lives in the daemon.

## Pattern: cron-scheduled sync

A real schedule (`*/5`), a longer timeout, and retries for a flaky upstream. This
is the shape most production integrations take.

```text
integrations/nightly-orders/
├── otter.yaml
└── main.py
```

```yaml
# integrations/nightly-orders/otter.yaml
version: 1

name: nightly-orders
description: Pull yesterday's orders every 5 minutes and checkpoint progress.

entrypoint: main.py

trigger:
  cron: "*/5 * * * *"

timeout: 5m
concurrency: 1

retry:
  attempts: 4
  backoff: exponential
  initial_delay: 5s
  max_delay: 2m

env:
  ORDERS_API: https://api.example.com/v1/orders
  PAGE_SIZE: "100"

secrets:
  - ORDERS_API_TOKEN
```

```python
# integrations/nightly-orders/main.py
import json
import os
import urllib.request

from otter import run

API = os.environ["ORDERS_API"]
TOKEN = os.environ["ORDERS_API_TOKEN"]
PAGE_SIZE = int(os.environ.get("PAGE_SIZE", "100"))


def fetch_page(cursor: str | None) -> dict:
    url = f"{API}?limit={PAGE_SIZE}"
    if cursor:
        url += f"&after={cursor}"
    request = urllib.request.Request(url, headers={"Authorization": f"Bearer {TOKEN}"})
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)


@run
def main(ctx):
    cursor = ctx.state.get("cursor")
    ctx.log.info("starting orders sync", cursor=cursor)

    page = fetch_page(cursor)
    orders = page["orders"]
    ctx.log.info("fetched page", count=len(orders))

    # ... deliver each order here; a real integration would retry per order ...

    if page.get("next_cursor"):
        ctx.state.set("cursor", page["next_cursor"])
        ctx.log.info("checkpoint advanced", cursor=page["next_cursor"])
    else:
        ctx.state.delete("cursor")
        ctx.log.info("sync complete, checkpoint cleared")

    if page.get("errors"):
        # A non-zero exit triggers the retry policy; the checkpoint makes the
        # retry resume instead of refetching everything.
        raise SystemExit(f"upstream reported {page['errors']} errors")
```

Run it manually once instead of waiting for the schedule:

```bash
otter run nightly-orders
otter runs --integration nightly-orders --limit 5
otter state get nightly-orders cursor
```

Expected:

```console
$ otter run nightly-orders
run_01HZYB1C2D3E4F5G6H7I8J9K0L1

$ otter logs run_01HZYB1C2D3E4F5G6H7I8J9K0L1
run started (attempt 1 of 4, trigger manual)
{"level":"info","event":"starting orders sync","cursor":null}
{"level":"info","event":"fetched page","count":100}
{"level":"info","event":"checkpoint advanced","cursor":"eyJvZmZzZXQiOjEwMH0="}
run succeeded (attempt 1, 1.204s), exit code 0
```

The `{"level":...}` lines are `ctx.log` calls: the SDK posts them to the daemon,
which stores them in the `otter` stream alongside the runtime's own lifecycle
events (queued, started, retries, finished). `otter logs` prints the `stdout`
and `otter` streams to standard output and the `stderr` stream to standard
error, so piping `otter logs` captures the integration's normal output without
losing the runtime's account of what happened.

Notes:

- The `ORDERS_API_TOKEN` secret must be in the **daemon's** environment. If it is
  missing, the run fails before Python starts and is not retried:
  `integration nightly-orders requires secrets that are not available: ORDERS_API_TOKEN`.
- Missed ticks during downtime are not replayed. The `cursor` checkpoint is what
  makes that safe: the next tick continues from where the last one stopped.

## Pattern: webhook-triggered integration

An integration that runs when an external system says so, with retries and
enough concurrency for bursts. The hook enqueues and returns immediately, so the
caller never waits for the work.

```text
integrations/order-events/
├── otter.yaml
└── main.py
```

```yaml
# integrations/order-events/otter.yaml
version: 1

name: order-events
description: Handle order webhooks from the storefront.

entrypoint: main.py

trigger:
  webhook:
    enabled: true

timeout: 120
concurrency: 4

retry:
  attempts: 5
  backoff: exponential
  initial_delay: 1s
  max_delay: 30s

secrets:
  - WAREHOUSE_TOKEN
```

```python
# integrations/order-events/main.py
import json
import os
import urllib.request

from otter import run

WAREHOUSE = "https://warehouse.internal.example.com/orders"
TOKEN = os.environ["WAREHOUSE_TOKEN"]


def forward(order: dict) -> None:
    body = json.dumps(order).encode()
    request = urllib.request.Request(
        WAREHOUSE,
        data=body,
        method="POST",
        headers={"Authorization": f"Bearer {TOKEN}", "Content-Type": "application/json"},
    )
    with urllib.request.urlopen(request, timeout=20) as response:
        if response.status >= 300:
            raise RuntimeError(f"warehouse returned HTTP {response.status}")


@run
def main(ctx):
    order = ctx.trigger.body or {}
    event = ctx.trigger.header("X-Event-Type") or ctx.trigger.metadata.get("event")

    ctx.log.info("received webhook", event=event, order_id=order.get("order_id"))

    if not order.get("order_id"):
        # Bad payload: fail fast, no point retrying.
        raise SystemExit("payload is missing order_id")

    forward(order)
    ctx.log.info("forwarded to warehouse", order_id=order["order_id"])
```

Get the token the daemon generated for this integration, then deliver a hook:

```bash
export OTTER_API_URL=http://127.0.0.1:7337
export OTTER_API_TOKEN=...        # only needed if the daemon requires it

TOKEN=$(curl -s -H "Authorization: Bearer $OTTER_API_TOKEN" \
  "$OTTER_API_URL/v1/integrations/order-events" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["webhook_token"])')

curl -s -X POST \
  -H "X-Otter-Token: $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'X-Event-Type: order.created' \
  -d '{"order_id": 4242, "total": 199.99}' \
  "$OTTER_API_URL/v1/hooks/order-events"
```

Expected hook response — `202 Accepted`, and only an acknowledgement, not a
result:

```json
{
  "run_id": "run_01HZYC1D2E3F4G5H6I7J8K9L0M1",
  "status": "queued"
}
```

```console
$ otter logs run_01HZYC1D2E3F4G5H6I7J8K9L0M1
run started (attempt 1 of 5, trigger webhook)
{"level":"info","event":"received webhook","event":"order.created","order_id":4242}
{"level":"info","event":"forwarded to warehouse","order_id":4242}
run succeeded (attempt 1, 612ms), exit code 0
```

Notes:

- Requests to a disabled or unknown hook return `404`; a wrong token returns
  `401`. See [security.md](security.md#webhook-authentication) for token
  handling and rotation.
- With `concurrency: 4`, four of these run at once and the rest queue. The hook
  still returns `202` immediately for every one of them.
- The webhook token is per integration and is regenerated if you
  `DELETE FROM webhook_tokens WHERE integration_id='order-events';` and restart.
  Callers must be updated when you do.

## Notes on the shipped examples

Neither shipped example is a "reference architecture" — they are deliberately
tiny, stdlib-only programs that fit in a screenshot:

| Example | Trigger | Demonstrates | Shipped directory |
| --- | --- | --- | --- |
| `counter` | `cron: "0 * * * *"` | Scheduling, durable state, logs, run history, `concurrency: 1` | `examples/counter/` |
| `customer-sync` | manual or webhook (no trigger configured) | Checkpointed sync, crash resume (`CRASH_AFTER`), retries (`MOCK_FAIL_AFTER`) | `examples/customer-sync/` |
| hello world | manual | Minimum manifest, `Context.from_environment()`, state | not shipped (above) |
| nightly orders | `cron: "*/5 * * * *"` | Secrets, `${VAR}` env, checkpoint on failure | not shipped (above) |
| order events | webhook | Webhook token, `ctx.trigger`, burst concurrency | not shipped (above) |

The three unshipped patterns live here rather than in `examples/` so that
`--integrations ./examples` stays small: fewer integrations means less scheduler
noise when you are following the README, and less Python to read before you
understand the runtime.
