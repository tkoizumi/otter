# Integration patterns

These are the shapes an integration takes. They are patterns rather than shipped
files: `otter init` generates a working starting point, and the runtime ships no
example catalog, so what you copy is the scaffold the project actually tests.

- [Pattern: hello world (manual only)](#pattern-hello-world-manual-only)
- [Pattern: cron-scheduled sync](#pattern-cron-scheduled-sync)
- [Pattern: webhook-triggered integration](#pattern-webhook-triggered-integration)

Every one of them ends the same way: **release before you run**. A run executes
the integration's active release rather than its source tree, so `otter run`
refuses until the integration has been released once. After an edit, release
again or the run keeps executing the previous snapshot.

```bash
otter release --all          # every integration in the workspace
otter release order-events   # or one at a time, by label or path
```

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
  `DELETE FROM webhook_tokens WHERE integration_id='order-events';` and run
  `otter reload`. Callers must be updated when you do.
