# otter_connectors

Shared Python client code for Otter integrations: a Shopify Admin API (GraphQL)
client and a Salesforce REST client, plus the small pieces any incremental sync
needs.

```python
from otter_connectors.checkpoint import Watermark
from otter_connectors.clients import salesforce_client, shopify_client

shopify = shopify_client("your-store.myshopify.com")                 # SHOPIFY_*
salesforce = salesforce_client("https://your-org.my.salesforce.com")  # SALESFORCE_*
```

## Why this is not in the Otter SDK

`otter` (in `sdk/python`) is the **runtime** SDK: state, logs, triggers,
checkpoints. It is embedded into the `otterd` binary and versioned with it.

This package is a **vendor** library. A Shopify client is not a runtime
primitive, so it does not belong in the daemon — Otter's test for adding a
primitive is "does almost every reliable integration need this?", and a vendor
client fails it. Keeping it here means:

- the daemon stays small, and carries no vendor code at all;
- you can read, fork and pin this code on your own schedule, rather than waiting
  for a runtime release;
- a second Shopify or Salesforce integration imports it instead of copying
  900 lines of client code.

## Using it from an integration

Declare the directory in the manifest. Otter resolves it relative to the
integration directory and prepends it to the child's `PYTHONPATH`, so there is
no install step and `otter validate` checks that it exists:

```yaml
python:
  executable: python3
  path:
    - ../../lib/python
```

`python.path` entries may also be absolute. Ordering on `PYTHONPATH` is: the
runtime SDK first (so `import otter` always resolves to the daemon's own copy),
then these directories, then whatever the operator already had.

Alternatively, install it (`pip install ./lib/python`) or add the directory to
`PYTHONPATH` yourself — the package has no third-party dependencies either way.

## What is in here

| Module | Contents |
| --- | --- |
| `shopify` | `ShopifyClient` — client credentials grant (24 hour tokens, renewed per run), GraphQL execution with throttle-aware backoff, Relay cursor paging. `numeric_id` for turning a GID into the trailing id. `ShopifyError`. |
| `salesforce` | `SalesforceClient` — OAuth (client credentials or password), batched upsert by External ID with a per-record fallback, picklist-aware value matching, non-fatal address rejection. `SalesforceError`, `SalesforceRecordError`, `pick_allowed`. |
| `clients` | `shopify_client` / `salesforce_client` — build the two clients above from the standard `SHOPIFY_*` and `SALESFORCE_*` settings. The store and the instance URL are required arguments, because each client is bound to exactly one target. |
| `records` | `build_record` and friends — the mechanics of turning a source document into an API payload (dotted paths, strip, drop empties, truncate, join), so an integration's field mapping can be **data** with callables as the escape hatch. |
| `checkpoint` | `Watermark` — a resumable "what have I already processed?" position in Otter state: the committed watermark only advances once a window drains, and an interrupted window resumes from its page cursor. |
| `config` | `env`, `env_int`, `env_bool`, `require_env` readers. |
| `http` | The CA-aware opener the clients share (see below). |
| `timeutil` | `utcnow`, `to_iso`, `parse_iso`. |
| `errors` | `ConnectorError`, `ConfigError`. |

`otter_schema` sits alongside it: schema references, so a mapping names fields
that were pulled from the system rather than typed from memory. See
[otter_schema/README.md](otter_schema/README.md).

## Two things these clients handle that are easy to miss

**TLS on a developer Mac.** The python.org macOS installers ship no CA store, so
every HTTPS request fails with `CERTIFICATE_VERIFY_FAILED` until Apple's
"Install Certificates.command" is run (which needs sudo). `http` falls back to
`certifi`, already installed alongside that interpreter, so integrations work
without it.

**`Accept: application/json` on the Shopify token endpoint.** Shopify renders
that endpoint's errors as HTML, and asking for JSON makes some failures come
back with an *empty* body — which is how a real problem turns into a blank error
message. The client asks for the rich page and extracts the reason from either
shape.

## Tests

```bash
PYTHONPATH=lib/python python3 -m unittest discover -s lib/python/tests
```

These cover the pure logic and the request shapes the clients must produce. The
end-to-end path (scheduling, state, retries) is covered by the integration
itself.
