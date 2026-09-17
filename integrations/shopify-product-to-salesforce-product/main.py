"""Pull Shopify products into Salesforce every five minutes.

A table of contents rather than a program: what to *read* is in ``source.py``,
where it *lands* is in ``mapping.py``, what it is *configured* to do is in
``settings.py``, the page loop and its sink are in ``product_sync.py``, the
resumable window in ``sync_window.py``, and what each run records about itself
in ``run_state.py``. This file reads no environment variables and holds no
durable state of its own.

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
from product_sync import DryRunSink, drain
from run_state import record_rejections, report_run, report_start
from settings import Settings
from sync_window import begin_window, finalize_window


@run
def main(ctx):
    cfg = Settings.load()

    # The store is an explicit argument rather than something the factory reads:
    # a Shopify client is bound to one store, so naming it here is what would
    # make a future multi-store run a loop instead of a rewrite. Credentials are
    # per-app, so those the factory does read. SALESFORCE_BATCH_SIZE configures
    # the client rather than this sync, so salesforce_client reads it too.
    shopify = shopify_client(cfg.shopify_store)
    # A dry run gets a sink with the same interface, so the loop has no branch
    # and there is no None to guard.
    salesforce = (
        DryRunSink(ctx.log)
        if cfg.dry_run
        else salesforce_client(cfg.salesforce_instance_url)
    )

    # Fail on a malformed mapping before touching any data, rather than on
    # whichever record happens to hit it first.
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

    result = drain(ctx.log, cfg, shopify, salesforce, mapping, window, deadline=deadline)

    # Close the window before reporting it, so the summary names what was
    # actually persisted.
    finalize_window(ctx.log, window, result, started)
    report_run(ctx, result, started, window.start, dry_run=cfg.dry_run)
    record_rejections(ctx, result.failures, written=result.written)
