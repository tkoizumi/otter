"""Pull Shopify products into Salesforce every five minutes.

Orchestration only. What this integration *reads* lives in ``source.py`` and
where it *lands* lives in ``mapping.py`` -- both are edited far more often than
this file, which should not need touching to add a field.

The reusable pieces -- the Shopify and Salesforce clients, the resumable
watermark, the record builder -- live in ``otter_connectors`` (see
``lib/python/README.md``). The runtime primitives (scheduling, retries,
timeouts, durable state, logs, run history) are Otter's, and are not
reimplemented here.
"""

import json
import time

from otter import run
from otter_connectors.checkpoint import Watermark
from otter_connectors.config import env, env_bool, env_int, require_env
from otter_connectors.records import build_record, validate_mapping
from otter_connectors.salesforce import SalesforceClient
from otter_connectors.shopify import ShopifyClient, ShopifyError
from otter_connectors.timeutil import parse_iso, to_iso, utcnow

from mapping import MAX_FIELD_LENGTH, variant_mapping
from source import fetch_page

#: How many rejected records to keep for inspection.
MAX_DLQ_ENTRIES = 100


@run
def main(ctx):
    store = require_env("SHOPIFY_STORE")
    external_id_field = env("SALESFORCE_EXTERNAL_ID_FIELD", "Shopify_Customer_Id__c")
    sobject = env("SALESFORCE_OBJECT", "Contact")

    page_size = env_int("PAGE_SIZE", 100)
    max_pages = env_int("MAX_PAGES_PER_RUN", 20)
    overlap_seconds = env_int("OVERLAP_SECONDS", 600)
    budget_seconds = env_int("RUN_BUDGET_SECONDS", 240)
    batch_size = env_int("SALESFORCE_BATCH_SIZE", 200)
    dry_run = env_bool("DRY_RUN", False)

    shopify = ShopifyClient(
        store=store,
        api_version=env("SHOPIFY_API_VERSION", "2026-07"),
        # Only set for a legacy pre-generated token; otherwise the client
        # credentials grant supplies a fresh 24 hour token per run.
        token=env("SHOPIFY_ACCESS_TOKEN"),
        client_id=env("SHOPIFY_CLIENT_ID"),
        client_secret=env("SHOPIFY_CLIENT_SECRET"),
        api_base=env("SHOPIFY_API_BASE"),
        token_url=env("SHOPIFY_TOKEN_URL"),
    )
    salesforce = (
        None
        if dry_run
        else SalesforceClient(
            instance_url=require_env("SALESFORCE_INSTANCE_URL"),
            api_version=env("SALESFORCE_API_VERSION", "62.0"),
            auth=env("SALESFORCE_AUTH", "client_credentials"),
            client_id=env("SALESFORCE_CLIENT_ID"),
            client_secret=env("SALESFORCE_CLIENT_SECRET"),
            username=env("SALESFORCE_USERNAME"),
            password=env("SALESFORCE_PASSWORD"),
            batch_size=batch_size,
        )
    )

    # Fail on a malformed mapping before touching any data, rather than on
    # whichever record happens to hit it first.

    started = utcnow()
    deadline = time.monotonic() + budget_seconds

    watermark = Watermark(
        ctx.state,
        overlap_seconds=overlap_seconds,
        backfill_from=parse_iso(env("BACKFILL_FROM")),
        lookback_days=env_int("BACKFILL_DAYS", 30),
    )
    had_watermark = watermark.committed() is not None
    window_start, cursor, resumed = watermark.begin(started)

    if resumed:
        ctx.log.info("resuming interrupted window", window_start=to_iso(window_start))
    elif not had_watermark:
        ctx.log.info("first run; backfilling", window_start=to_iso(window_start))

    ctx.log.info(
        "sync starting",
        store=store,
        object=sobject,
        window_start=to_iso(window_start),
        dry_run=dry_run,
        page_size=page_size,
    )

    # ---- drain the window ------------------------------------------------ #
    fetched = written = 0
    failures = []
    pages = 0
    complete = False

    while pages < max_pages:
        if time.monotonic() > deadline:
            ctx.log.warning(
                "run budget reached; will continue on the next tick",
                pages=pages,
                fetched=fetched,
                budget_seconds=budget_seconds,
            )
            break

        products, page_info = fetch_page(
            shopify,
            window_start=window_start,
            page_size=page_size,
            cursor=cursor,
            sort_key=env("SHOPIFY_SORT_KEY", "UPDATED_AT"),
        )
        ctx.log.info(
            "shopify page",
            page=pages,
            count=len(products),
            first=products[0] if products else None,
        )
        pages += 1

        if not products:
            complete = True
            break

        records = []

        for product in products:
            variants = product.get("variants").get("nodes")
            for variant in variants:
                ctx.log.info(
                    "variant",
                    variant=variant,
                )
                mapping = variant_mapping()

                variant["product_id"] = product.get("id")
                variant_record = build_record(mapping, variant, limits=MAX_FIELD_LENGTH)
                records.append(variant_record)

        ctx.log.info(
            "records",
            records=records,
            count=len(records),
            first=records[0] if records else None,
        )
        fetched += len(records)

        if dry_run:
            for record in records[:3]:
                ctx.log.info("dry run: would upsert", record=record)
            page_written = len(records)
            ctx.log.info("dry run: page skipped", page=pages, records=len(records))
        else:
            page_written, page_failures = salesforce.upsert(
                sobject, external_id_field, records
            )
            failures.extend(page_failures)
        written += page_written

        next_cursor = page_info.get("endCursor")
        if not page_info.get("hasNextPage"):
            complete = True
            break
        if not next_cursor or next_cursor == cursor:
            raise ShopifyError(
                "Shopify returned a non-advancing page cursor; aborting to avoid a loop"
            )

        cursor = next_cursor
        # Persist after every page: a crash here resumes at this page rather
        # than replaying the whole window.
        watermark.save_cursor(cursor)
        ctx.log.info(
            "page synced", page=pages, records=len(records), written=page_written
        )

    # ---- commit or hand over -------------------------------------------- #
    if complete:
        if dry_run:
            ctx.log.info(
                "dry run: watermark not advanced", window_start=to_iso(window_start)
            )
        else:
            watermark.commit(started)
    else:
        watermark.save_cursor(cursor)
        ctx.log.info(
            "window partially drained; next run continues", cursor=cursor, pages=pages
        )

    if not dry_run and salesforce is not None and salesforce.address_fallbacks:
        ctx.log.warning(
            "wrote some products without their mailing address: the org's "
            "State/Country picklist rejected the value",
            count=salesforce.address_fallbacks,
            first_reason=salesforce.address_fallback_reason,
            hint="align the State/Country picklist values, or set SYNC_ADDRESS=0 to skip addresses",
        )

    if failures:
        _record_failures(ctx, failures)

    ctx.state.set(
        "last_run",
        {
            "started_at": to_iso(started),
            "finished_at": to_iso(utcnow()),
            "window_start": to_iso(window_start),
            "pages": pages,
            "fetched": fetched,
            "written": written,
            "failed": len(failures),
            "complete": complete,
            "dry_run": dry_run,
        },
    )

    ctx.log.info(
        "sync finished",
        pages=pages,
        fetched=fetched,
        written=written,
        failed=len(failures),
        complete=complete,
    )

    if failures:
        # Naming the first reason here matters: when every record fails it is
        # almost always configuration (the external ID field not flagged as an
        # External ID, a duplicate rule, a validation rule, a required field),
        # and the count alone tells you nothing.
        ctx.log.warning(
            "some products were rejected by Salesforce and recorded in state",
            failed=len(failures),
            state_key="failed_products",
            written=written,
            first_customer_id=failures[0][0],
            first_error=failures[0][1],
        )
        for customer_id, message in failures[:3]:
            ctx.log.warning(
                "customer rejected", shopify_customer_id=customer_id, error=message
            )


def _record_failures(ctx, failures):
    """Keep a bounded record of permanent per-record failures.

    They are deliberately not raised: a single malformed customer must not stop
    the other 2000 from syncing, and the watermark should still advance.

    Inspect with: otter state get shopify-to-salesforce failed_products
    """
    existing = ctx.state.get("failed_products") or []
    if not isinstance(existing, list):
        existing = []
    stamp = to_iso(utcnow())

    merged = existing + [
        {
            "shopify_customer_id": str(customer_id),
            "error": str(message)[:500],
            "at": stamp,
        }
        for customer_id, message in failures
    ]
    ctx.state.set("failed_products", merged[-MAX_DLQ_ENTRIES:])
    ctx.state.set("failed_total", (ctx.state.get("failed_total") or 0) + len(failures))
