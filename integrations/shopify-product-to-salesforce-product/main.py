"""Pull Shopify products into Salesforce every five minutes.

Orchestration only. What this integration *reads* lives in ``source.py`` and
where it *lands* lives in ``mapping.py`` -- both are edited far more often than
this file, which should not need touching to add a field. What it is
*configured* to do lives in ``settings.py``; this file reads no environment
variables itself.

The reusable pieces -- the Shopify and Salesforce clients, the resumable
watermark, the record builder -- live in ``otter_connectors`` (see
``lib/python/README.md``). The runtime primitives (scheduling, retries,
timeouts, durable state, logs, run history) are Otter's, and are not
reimplemented here.
"""

import time

from otter import run
from otter_connectors.clients import salesforce_client, shopify_client
from otter_connectors.records import validate_mapping
from otter_connectors.timeutil import utcnow

from mapping import variant_mapping
from product_sync import query_products_and_upsert_to_sf
from reporting import record_rejections, report_run, report_start
from settings import Settings
from sync_window import begin_window, finalize_window


@run
def main(ctx):
    cfg = Settings.load()
    shopify = shopify_client(cfg.shopify_store)
    salesforce = None if cfg.dry_run else salesforce_client(cfg.salesforce_instance_url)

    mapping = validate_mapping(variant_mapping())

    started = utcnow()
    deadline = time.monotonic() + cfg.budget_seconds

    window = begin_window(ctx.state, cfg, started)
    report_start(
        ctx.log,
        cfg,
        window.start,
        resumed=window.resumed,
        had_watermark=window.had_watermark,
    )

    result = query_products_and_upsert_to_sf(
        ctx.log,
        cfg,
        shopify,
        salesforce,
        mapping,
        window.watermark,
        window_start=window.start,
        cursor=window.cursor,
        deadline=deadline,
    )
    finalize_window(ctx.log, window, result, started, dry_run=cfg.dry_run)
    record_rejections(ctx, result.failures, written=result.written)
    report_run(ctx, result, started, window.start, dry_run=cfg.dry_run)
