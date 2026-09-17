"""Tests for ``main``: the loop it runs and the state it writes.

Nothing pinned the orchestration before this file -- ``test_source.py`` covers
what is read and ``test_settings.py`` covers what is configured, but the page
loop, the watermark handover and the shape of a failure record were only ever
exercised by a real run against two live APIs. These are characterization
tests: they describe what ``main`` does *today*, so the loop can be extracted
later without quietly changing it.

They drive the real ``main`` with a stub context and stub clients. ``@run`` is a
no-op when ``OTTER_RUN_ID`` is unset (``sdk/python/otter/runner.py``), so
``main(ctx)`` is an ordinary function here.
"""

import os
import sys
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
INTEGRATION_DIR = os.path.dirname(HERE)
REPO_ROOT = os.path.dirname(os.path.dirname(INTEGRATION_DIR))

# sdk/python for `otter`, lib/python for the connectors, the integration
# directory for `main`, `source` and `mapping`.
for path in (
    os.path.join(REPO_ROOT, "sdk", "python"),
    os.path.join(REPO_ROOT, "lib", "python"),
    INTEGRATION_DIR,
):
    if path not in sys.path:
        sys.path.insert(0, path)

import main as integration  # noqa: E402
import product_sync  # noqa: E402
from reporting import MAX_DLQ_ENTRIES  # noqa: E402
from otter_connectors.shopify import ShopifyError  # noqa: E402

#: A final page: no more products after it.
END = {"hasNextPage": False, "endCursor": "end"}


class State:
    """The slice of ``ctx.state`` that ``main`` touches."""

    def __init__(self, values=None):
        self.values = dict(values or {})

    def get(self, key):
        return self.values.get(key)

    def set(self, key, value):
        self.values[key] = value

    def delete(self, key):
        self.values.pop(key, None)


class Log:
    def __init__(self):
        self.entries = []

    def info(self, event, **fields):
        self.entries.append(("info", event, fields))

    def warning(self, event, **fields):
        self.entries.append(("warning", event, fields))

    def error(self, event, **fields):
        self.entries.append(("error", event, fields))


class Ctx:
    def __init__(self, values=None):
        self.state = State(values)
        self.log = Log()


class Sink:
    """Stands in for ``SalesforceClient``: takes records, reports failures."""

    def __init__(self, failures=()):
        self.batches = []
        self.failures = list(failures)

    def upsert(self, sobject, external_id_field, records):
        self.batches.append((sobject, external_id_field, list(records)))
        return len(records) - len(self.failures), list(self.failures)


def variant(variant_id, title="Default Title"):
    return {"id": "gid://shopify/ProductVariant/%s" % variant_id, "title": title}


def product(product_id, variant_ids, page=None, title="The Hidden Snowboard"):
    """A product in the shape the query returns, nested connection and all."""
    return {
        "id": "gid://shopify/Product/%s" % product_id,
        "title": title,
        # Single-variant products get the "Default Title" placeholder, so the
        # mapped Name is the product's title. See mapping.variant_name.
        "hasOnlyDefaultVariant": True,
        "variants": {
            "nodes": [variant(variant_id) for variant_id in variant_ids],
            "pageInfo": dict(page or END),
        },
    }


class Harness:
    """Drives the real ``main`` against staged pages and stub clients.

    ``pages`` are ``(products, page_info)`` pairs as ``fetch_page`` returns
    them, in the order ``main`` should receive them.
    """

    def __init__(self, *pages, dry_run=True, failures=(), env=None, state=None):
        self.ctx = Ctx(state)
        self.staged = list(pages)
        self.failures = list(failures)
        self.stores = []
        self.sinks = []
        self.cursors = []
        self.environ = {
            "SHOPIFY_STORE": "example.myshopify.com",
            "SALESFORCE_INSTANCE_URL": "https://example.my.salesforce.com",
            "DRY_RUN": "1" if dry_run else "0",
        }
        self.environ.update(env or {})

    def fetch_page(self, shopify, window_start, page_size, cursor=None):
        self.cursors.append(cursor)
        if not self.staged:
            raise AssertionError("main asked for more pages than the test staged")
        return self.staged.pop(0)

    def shopify_client(self, store, **kwargs):
        self.stores.append(store)
        return mock.Mock()

    def salesforce_client(self, instance_url, **kwargs):
        sink = Sink(self.failures)
        self.sinks.append(sink)
        return sink

    def run(self):
        with mock.patch.dict(os.environ, self.environ, clear=True), \
                mock.patch.object(product_sync, "fetch_page", self.fetch_page), \
                mock.patch.object(integration, "shopify_client", self.shopify_client), \
                mock.patch.object(
                    integration, "salesforce_client", self.salesforce_client):
            integration.main(self.ctx)
        return self.ctx


class Wiring(unittest.TestCase):
    """What main builds, and for which target."""

    def test_the_client_is_built_for_the_configured_store(self):
        harness = Harness(([], END))
        harness.run()
        self.assertEqual(harness.stores, ["example.myshopify.com"])

    def test_a_dry_run_builds_no_salesforce_client(self):
        harness = Harness(([], END))
        harness.run()
        self.assertEqual(harness.sinks, [], "dry run must not reach Salesforce")

    def test_the_mapped_records_reach_the_configured_object(self):
        harness = Harness(([product(9, [555])], END), dry_run=False)
        harness.run()
        sobject, external_id_field, records = harness.sinks[0].batches[0]
        self.assertEqual(sobject, "Product2")
        self.assertEqual(external_id_field, "Shopify_Variant_Id__c")
        self.assertEqual(records[0]["Shopify_Product_Id__c"], "9")
        self.assertEqual(records[0]["Shopify_Variant_Id__c"], "555")
        self.assertEqual(records[0]["Name"], "The Hidden Snowboard")


class DryRun(unittest.TestCase):

    def test_it_does_not_advance_the_watermark(self):
        ctx = Harness(([product(9, [555])], END)).run()
        self.assertIsNone(ctx.state.get("sync_cursor"))
        self.assertIsNone(ctx.state.get("in_progress_cursor"))

    def test_it_still_reports_what_it_would_have_written(self):
        ctx = Harness(([product(9, [555, 556])], END)).run()
        last = ctx.state.get("last_run")
        self.assertEqual(last["fetched"], 2)
        self.assertEqual(last["written"], 2)
        self.assertTrue(last["complete"])
        self.assertTrue(last["dry_run"])


class Window(unittest.TestCase):

    def test_a_complete_window_commits_and_clears_the_window_keys(self):
        ctx = Harness(([product(9, [555])], END), dry_run=False).run()
        self.assertIsNotNone(ctx.state.get("sync_cursor"))
        self.assertIsNone(ctx.state.get("in_progress_window_start"))
        self.assertIsNone(ctx.state.get("in_progress_cursor"))

    def test_an_empty_page_completes_the_window(self):
        ctx = Harness(([], {"hasNextPage": True, "endCursor": "c"}), dry_run=False).run()
        last = ctx.state.get("last_run")
        self.assertEqual(last["pages"], 1)
        self.assertTrue(last["complete"], "an empty page means the window is drained")

    def test_a_partial_window_keeps_its_cursor_and_the_old_watermark(self):
        ctx = Harness(
            ([product(9, [555])], {"hasNextPage": True, "endCursor": "c1"}),
            ([product(9, [556])], END),
            dry_run=False,
            env={"MAX_PAGES_PER_RUN": "1"},
        ).run()
        self.assertEqual(ctx.state.get("in_progress_cursor"), "c1")
        self.assertIsNone(ctx.state.get("sync_cursor"))
        self.assertFalse(ctx.state.get("last_run")["complete"])

    def test_a_resumed_window_pages_on_from_the_stored_cursor(self):
        harness = Harness(
            ([product(9, [555])], END),
            state={
                "in_progress_window_start": "2026-01-01T00:00:00+00:00",
                "in_progress_cursor": "stored",
            },
        )
        ctx = harness.run()
        self.assertEqual(harness.cursors, ["stored"])
        self.assertTrue(ctx.state.get("last_run")["complete"])

    def test_the_budget_stops_the_run_before_the_first_page(self):
        harness = Harness(dry_run=False, env={"RUN_BUDGET_SECONDS": "0"})
        ctx = harness.run()
        self.assertEqual(harness.cursors, [], "the budget is checked before fetching")
        self.assertEqual(ctx.state.get("last_run")["pages"], 0)
        self.assertFalse(ctx.state.get("last_run")["complete"])

    def test_a_page_that_does_not_advance_is_an_error_not_a_loop(self):
        harness = Harness(
            ([product(9, [555])], {"hasNextPage": True, "endCursor": "same"}),
            ([product(9, [556])], {"hasNextPage": True, "endCursor": "same"}),
            dry_run=False,
        )
        with self.assertRaises(ShopifyError):
            harness.run()


class FailureRecording(unittest.TestCase):

    def test_a_rejected_record_is_recorded_and_does_not_hold_the_window(self):
        ctx = Harness(
            ([product(9, [555, 556])], END),
            dry_run=False,
            failures=[("555", "REQUIRED_FIELD_MISSING: Name")],
        ).run()
        failed = ctx.state.get("failed_products")
        self.assertEqual(len(failed), 1)
        self.assertEqual(failed[0]["shopify_variant_id"], "555")
        self.assertIn("REQUIRED_FIELD_MISSING", failed[0]["error"])
        self.assertIn("at", failed[0])
        self.assertEqual(ctx.state.get("failed_total"), 1)
        self.assertTrue(ctx.state.get("last_run")["complete"])
        self.assertIsNotNone(ctx.state.get("sync_cursor"))

    def test_the_stored_list_is_bounded(self):
        ctx = Harness(
            ([product(9, [555])], END),
            dry_run=False,
            failures=[("555", "nope")],
            state={
                "failed_products": [
                    {"shopify_variant_id": str(n)} for n in range(MAX_DLQ_ENTRIES)
                ]
            },
        ).run()
        self.assertEqual(
            len(ctx.state.get("failed_products")), MAX_DLQ_ENTRIES)

    def test_a_corrupt_list_in_state_is_replaced_rather_than_fatal(self):
        ctx = Harness(
            ([product(9, [555])], END),
            dry_run=False,
            failures=[("555", "nope")],
            state={"failed_products": "not a list"},
        ).run()
        self.assertEqual(len(ctx.state.get("failed_products")), 1)


if __name__ == "__main__":
    unittest.main()
