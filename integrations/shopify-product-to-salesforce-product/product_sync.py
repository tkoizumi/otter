"""Read Shopify products, map their variants, and upsert them to Salesforce."""

import time
from dataclasses import dataclass

from otter_connectors.records import build_record
from otter_connectors.shopify import ShopifyError

from source import fetch_page, variants_with_product


class DryRunSink:
    """Stands in for ``SalesforceClient``: reports what would be written.

    Taking the same ``upsert`` signature as the real client is what lets a dry
    run be a client rather than a flag threaded through the loop. There is no
    ``None`` for a later edit to trip over, and no branch in the page loop.
    """

    def __init__(self, log):
        self.log = log

    def upsert(self, sobject, external_id_field, records):
        for record in records[:3]:
            self.log.info("dry run: would upsert", record=record)
        self.log.info("dry run: page skipped", records=len(records))
        return len(records), []


@dataclass
class DrainResult:
    pages: int
    fetched: int
    written: int
    failures: list[tuple[str, str]]
    cursor: str | None
    complete: bool


def drain(log, cfg, shopify, salesforce, mapping, window, *, deadline,
          now=time.monotonic):
    """Process pages and checkpoint progress until drained or out of budget.

    ``now`` is injectable so the budget path is testable without sleeping.
    """
    window_start = window.start
    cursor = window.cursor
    fetched = written = 0
    failures = []
    pages = 0
    complete = False

    while pages < cfg.max_pages:
        if now() > deadline:
            log.warning(
                "run budget reached; will continue on the next tick",
                pages=pages,
                fetched=fetched,
                budget_seconds=cfg.budget_seconds,
            )
            break

        products, page_info = fetch_page(
            shopify,
            window_start=window_start,
            page_size=cfg.page_size,
            cursor=cursor,
        )
        pages += 1

        if not products:
            complete = True
            break

        records = _build_records(shopify, mapping, products)

        # The product itself is deliberately not logged: it now carries every
        # variant beneath it, so one page would write a few hundred nodes per
        # line. The records below are what you actually read.
        log.info(
            "shopify page",
            page=pages,
            products=len(products),
            variants=len(records),
            first=products[0].get("title"),
        )

        log.info(
            "records",
            records=records,
            count=len(records),
            first=records[0] if records else None,
        )
        fetched += len(records)

        page_written, page_failures = salesforce.upsert(
            cfg.salesforce_object, cfg.external_id_field, records
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
        # Checkpoint after writing: a crash after this resumes at the next page.
        # Whether that is durable is the window's decision, since a dry run
        # persists nothing.
        window.checkpoint(cursor)
        log.info("page synced", page=pages, records=len(records), written=page_written)

    return DrainResult(
        pages=pages,
        fetched=fetched,
        written=written,
        failures=failures,
        cursor=cursor,
        complete=complete,
    )


def _build_records(shopify, mapping, products):
    """Map every variant with its parent product attached."""
    return [
        build_record(mapping, variant)
        for product in products
        for variant in variants_with_product(shopify, product)
    ]
