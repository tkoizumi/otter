"""Tests for the Shopify side of this integration: the query and its paging.

``source.py`` and ``mapping.py`` have to stay in step -- a field the mapping
reads but the query never asks for becomes a quietly missing value -- so these
tests cover both the paging variables and the fields the query requests.
"""

import os
import sys
import unittest
from datetime import datetime, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
INTEGRATION_DIR = os.path.dirname(HERE)
REPO_ROOT = os.path.dirname(os.path.dirname(INTEGRATION_DIR))

for path in (os.path.join(REPO_ROOT, "lib", "python"),
             os.path.join(REPO_ROOT, "sdk", "python"),
             INTEGRATION_DIR):
    if path not in sys.path:
        sys.path.insert(0, path)

import source  # noqa: E402

WINDOW_START = datetime(2025, 12, 1, tzinfo=timezone.utc)


class FakeShopify:
    """Stands in for ShopifyClient, capturing what it was asked for."""

    def __init__(self, nodes=None, page_info=None):
        self.calls = []
        self.nodes = nodes if nodes is not None else []
        self.page_info = page_info or {"hasNextPage": False, "endCursor": None}

    def connection(self, query, variables, path="customers"):
        self.calls.append({"query": query, "variables": variables, "path": path})
        return self.nodes, self.page_info

    @property
    def last(self):
        return self.calls[-1]


class FetchPageTests(unittest.TestCase):
    def test_filters_by_the_window_start(self):
        shopify = FakeShopify()
        source.fetch_page(shopify, window_start=WINDOW_START, page_size=50)

        variables = shopify.last["variables"]
        self.assertEqual(variables["query"], "updated_at:>'2025-12-01T00:00:00Z'")
        self.assertEqual(variables["first"], 50)
        self.assertEqual(variables["sortKey"], "UPDATED_AT")
        self.assertEqual(shopify.last["path"], "customers")

    def test_omits_the_cursor_on_the_first_page(self):
        shopify = FakeShopify()
        source.fetch_page(shopify, window_start=WINDOW_START, page_size=10)
        self.assertIsNone(shopify.last["variables"]["after"])

    def test_passes_the_cursor_through_for_later_pages(self):
        shopify = FakeShopify()
        source.fetch_page(shopify, window_start=WINDOW_START, page_size=10, cursor="page-2")
        self.assertEqual(shopify.last["variables"]["after"], "page-2")

    def test_allows_the_sort_key_to_be_overridden(self):
        # Some API versions reject sortKey alongside a query filter.
        shopify = FakeShopify()
        source.fetch_page(shopify, window_start=WINDOW_START, page_size=10,
                          sort_key="CREATED_AT")
        self.assertEqual(shopify.last["variables"]["sortKey"], "CREATED_AT")

    def test_returns_the_page_and_its_paging_info(self):
        shopify = FakeShopify(nodes=[{"id": "1"}],
                              page_info={"hasNextPage": True, "endCursor": "c1"})
        nodes, page_info = source.fetch_page(shopify, window_start=WINDOW_START, page_size=1)

        self.assertEqual(nodes, [{"id": "1"}])
        self.assertEqual(page_info["endCursor"], "c1",
                         "the caller checkpoints endCursor")

    def test_updated_since_renders_the_shopify_filter(self):
        self.assertEqual(source.updated_since(WINDOW_START),
                         "updated_at:>'2025-12-01T00:00:00Z'")


class QueryShapeTests(unittest.TestCase):
    """The query must ask for everything the mapping reads."""

    #: Source fields the mapping in mapping.py depends on. Adding a mapped
    #: field means adding it to both files; this is the reminder.
    MAPPED_SOURCE_FIELDS = (
        "firstName", "lastName", "email", "phone",
        "address1", "address2", "city",
        "province", "provinceCode",
        "country", "countryCodeV2", "zip",
    )

    def test_asks_for_every_field_the_mapping_reads(self):
        for field in self.MAPPED_SOURCE_FIELDS:
            self.assertIn(field, source.CUSTOMERS_QUERY, field)

    def test_asks_for_the_id_the_upsert_key_comes_from(self):
        self.assertIn("id", source.CUSTOMERS_QUERY)

    def test_requests_cursor_paging(self):
        for fragment in ("pageInfo", "hasNextPage", "endCursor"):
            self.assertIn(fragment, source.CUSTOMERS_QUERY, fragment)

    def test_selects_the_customers_connection(self):
        self.assertIn("customers(", source.CUSTOMERS_QUERY)
        self.assertIn("sortKey: $sortKey", source.CUSTOMERS_QUERY)


if __name__ == "__main__":
    unittest.main()
