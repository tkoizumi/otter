"""Tests for ``settings.py``: the defaults, the required values, the errors.

The dataclass is what lets a test build a configuration without touching
``os.environ``; these tests cover the other half -- that ``load()`` reads the
environment the way ``.env.example`` and ``otter.yaml`` say it does, and that a
bad value fails here rather than half way through a sync.
"""

import os
import sys
import unittest
from dataclasses import FrozenInstanceError
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
INTEGRATION_DIR = os.path.dirname(HERE)
REPO_ROOT = os.path.dirname(os.path.dirname(INTEGRATION_DIR))

for path in (os.path.join(REPO_ROOT, "lib", "python"), INTEGRATION_DIR):
    if path not in sys.path:
        sys.path.insert(0, path)

from otter_connectors.errors import ConfigError  # noqa: E402
from otter_connectors.timeutil import parse_iso  # noqa: E402

from settings import Settings  # noqa: E402

#: The two settings with no default, so every ``load()`` needs them.
REQUIRED = {
    "SHOPIFY_STORE": "example.myshopify.com",
    "SALESFORCE_INSTANCE_URL": "https://example.my.salesforce.com",
}


def load(**overrides):
    """``Settings.load()`` with ``overrides`` on top of the required values."""
    environ = dict(REQUIRED)
    environ.update(overrides)
    with mock.patch.dict(os.environ, environ, clear=True):
        return Settings.load()


class Defaults(unittest.TestCase):

    def test_the_required_values_are_read(self):
        cfg = load()
        self.assertEqual(cfg.shopify_store, "example.myshopify.com")
        self.assertEqual(cfg.salesforce_instance_url, "https://example.my.salesforce.com")

    def test_the_defaults_are_the_ones_the_docs_state(self):
        """A change here is a change to the table in .env.example."""
        cfg = load()
        self.assertEqual(cfg.salesforce_object, "Product2")
        self.assertEqual(cfg.external_id_field, "Shopify_Variant_Id__c")
        self.assertEqual(cfg.page_size, 100)
        self.assertEqual(cfg.max_pages, 20)
        self.assertEqual(cfg.overlap_seconds, 600)
        self.assertEqual(cfg.budget_seconds, 240)
        self.assertEqual(cfg.backfill_days, 30)
        self.assertIsNone(cfg.backfill_from)
        self.assertFalse(cfg.dry_run)

    def test_every_knob_can_be_overridden(self):
        cfg = load(
            SALESFORCE_OBJECT="Product__c",
            SALESFORCE_EXTERNAL_ID_FIELD="Variant__c",
            PAGE_SIZE="7",
            MAX_PAGES_PER_RUN="3",
            OVERLAP_SECONDS="1",
            RUN_BUDGET_SECONDS="2",
            BACKFILL_DAYS="4",
            BACKFILL_FROM="2026-01-01T00:00:00Z",
            DRY_RUN="1",
        )
        self.assertEqual(cfg.salesforce_object, "Product__c")
        self.assertEqual(cfg.external_id_field, "Variant__c")
        self.assertEqual(cfg.page_size, 7)
        self.assertEqual(cfg.max_pages, 3)
        self.assertEqual(cfg.overlap_seconds, 1)
        self.assertEqual(cfg.budget_seconds, 2)
        self.assertEqual(cfg.backfill_days, 4)
        self.assertEqual(cfg.backfill_from, parse_iso("2026-01-01T00:00:00Z"))
        self.assertTrue(cfg.dry_run)

    def test_batch_size_is_not_a_setting(self):
        """It configures the client rather than the sync, so reading it is
        ``clients.salesforce_client``'s job and it never reaches Settings."""
        self.assertFalse(hasattr(load(SALESFORCE_BATCH_SIZE="5"), "batch_size"))


class Failures(unittest.TestCase):

    def test_a_missing_store_is_an_error(self):
        with mock.patch.dict(
            os.environ, {"SALESFORCE_INSTANCE_URL": "https://x"}, clear=True
        ):
            with self.assertRaises(ConfigError) as caught:
                Settings.load()
        self.assertIn("SHOPIFY_STORE", str(caught.exception))

    def test_a_missing_instance_url_is_an_error(self):
        with mock.patch.dict(os.environ, {"SHOPIFY_STORE": "x.myshopify.com"}, clear=True):
            with self.assertRaises(ConfigError) as caught:
                Settings.load()
        self.assertIn("SALESFORCE_INSTANCE_URL", str(caught.exception))

    def test_a_non_numeric_knob_names_itself(self):
        with self.assertRaises(ConfigError) as caught:
            load(PAGE_SIZE="lots")
        self.assertIn("PAGE_SIZE", str(caught.exception))
        self.assertIn("lots", str(caught.exception))


class Construction(unittest.TestCase):

    def test_a_settings_can_be_built_without_an_environment(self):
        """The reason this is a dataclass: a test constructs one directly."""
        cfg = Settings(
            shopify_store="s.myshopify.com",
            salesforce_instance_url="https://x",
            salesforce_object="Product2",
            external_id_field="Shopify_Variant_Id__c",
            page_size=1,
            max_pages=1,
            overlap_seconds=0,
            budget_seconds=1,
            backfill_from=None,
            backfill_days=0,
            dry_run=True,
        )
        self.assertTrue(cfg.dry_run)

    def test_it_is_frozen(self):
        """A run must not be able to retarget itself part way through."""
        with self.assertRaises(FrozenInstanceError):
            load().dry_run = True


if __name__ == "__main__":
    unittest.main()
