"""What a run records about itself: durable state, and the logs describing it.

"State" is in the name because this module *persists* as well as logs.
``report_run`` writes ``last_run`` and ``record_rejections`` writes
``failed_products`` and ``failed_total``; both are read back by
``otter state get`` and by ``make sync-status``, so they are part of the
integration's contract rather than just output.
"""

from otter_connectors.timeutil import to_iso, utcnow

#: How many rejected records to keep for inspection.
MAX_DLQ_ENTRIES = 100


def report_start(log, cfg, window_start, *, resumed, had_watermark):
    """Log the window's starting position and sync configuration."""
    if resumed:
        log.info("resuming interrupted window", window_start=to_iso(window_start))
    elif not had_watermark:
        log.info("first run; backfilling", window_start=to_iso(window_start))

    log.info(
        "sync starting",
        store=cfg.shopify_store,
        object=cfg.salesforce_object,
        window_start=to_iso(window_start),
        dry_run=cfg.dry_run,
        page_size=cfg.page_size,
    )


def report_window_progress(log, window_start, result, *, dry_run):
    """Report where the window ended up, and whether anything was persisted."""
    if dry_run:
        log.info("dry run: watermark not advanced", window_start=to_iso(window_start))
        if not result.complete:
            # Otherwise a rehearsal that stopped early reads like a complete one.
            log.info(
                "dry run: window not fully drained; nothing persists",
                cursor=result.cursor,
                pages=result.pages,
            )
        return

    if not result.complete:
        log.info(
            "window partially drained; next run continues",
            cursor=result.cursor,
            pages=result.pages,
        )


def report_run(ctx, result, started, window_start, *, dry_run):
    """Persist run metadata and log the same totals."""
    totals = {
        "pages": result.pages,
        "fetched": result.fetched,
        "written": result.written,
        "failed": len(result.failures),
        "complete": result.complete,
    }
    ctx.state.set(
        "last_run",
        {
            **totals,
            "started_at": to_iso(started),
            "finished_at": to_iso(utcnow()),
            "window_start": to_iso(window_start),
            "dry_run": dry_run,
        },
    )
    ctx.log.info("sync finished", **totals)


def record_rejections(ctx, failures, *, written):
    """Persist rejected records and log their first reasons."""
    if not failures:
        return

    _record_failures(ctx, failures)
    # Naming the first reason here matters: when every record fails it is
    # almost always configuration (the external ID field not flagged as an
    # External ID, a duplicate rule, a validation rule, a required field),
    # and the count alone tells you nothing.
    ctx.log.warning(
        "some products were rejected by Salesforce and recorded in state",
        failed=len(failures),
        state_key="failed_products",
        written=written,
        first_id=failures[0][0],
        first_error=failures[0][1],
    )
    for variant_id, message in failures[:3]:
        ctx.log.warning("record rejected", shopify_variant_id=variant_id, error=message)


def _record_failures(ctx, failures):
    """Keep a bounded record of permanent per-record failures.

    They are deliberately not raised: one product Salesforce will not accept
    must not stop the other 2000 from syncing, and the watermark should still
    advance.

    Inspect with:
        otter state get shopify-product-to-salesforce-product failed_products
    """
    existing = ctx.state.get("failed_products") or []
    if not isinstance(existing, list):
        existing = []
    stamp = to_iso(utcnow())

    merged = existing + [
        {
            "shopify_variant_id": str(variant_id),
            "error": str(message)[:500],
            "at": stamp,
        }
        for variant_id, message in failures
    ]
    ctx.state.set("failed_products", merged[-MAX_DLQ_ENTRIES:])
    ctx.state.set("failed_total", (ctx.state.get("failed_total") or 0) + len(failures))
