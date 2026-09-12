"""Unit tests for the shared connector library.

    PYTHONPATH=lib/python python3 -m unittest discover -s lib/python/tests

No daemon and no network beyond loopback: the end-to-end path (scheduling,
durable state, retries) is covered by the integration's own checks.
"""

import http.server
import json
import os
import sys
import threading
import unittest
from datetime import datetime, timedelta, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))

from otter_connectors.checkpoint import Watermark  # noqa: E402
from otter_connectors.config import env, env_bool, env_int, require_env  # noqa: E402
from otter_connectors.errors import ConfigError  # noqa: E402
from otter_connectors.salesforce import (  # noqa: E402
    SalesforceClient,
    combine_errors,
    is_address_picklist_error,
    pick_allowed,
)
from otter_connectors.timeutil import parse_iso, to_iso, utcnow  # noqa: E402

UTC = timezone.utc
NOW = datetime(2026, 9, 12, 12, 0, tzinfo=UTC)


class FakeState:
    """Stands in for the SDK's ctx.state."""

    def __init__(self, initial=None):
        self.data = dict(initial or {})

    def get(self, key, default=None):
        return self.data.get(key, default)

    def set(self, key, value):
        self.data[key] = value

    def delete(self, key):
        self.data.pop(key, None)


# --------------------------------------------------------------------------- #
# Watermark
# --------------------------------------------------------------------------- #


class WatermarkTests(unittest.TestCase):
    def test_first_run_starts_at_the_backfill_point(self):
        state = FakeState()
        backfill = datetime(2025, 1, 1, tzinfo=UTC)
        watermark = Watermark(state, overlap_seconds=600, backfill_from=backfill)

        start, cursor, resumed = watermark.begin(NOW)

        self.assertEqual(start, backfill)
        self.assertIsNone(cursor)
        self.assertFalse(resumed)
        # The window is recorded so an interrupted run can resume it.
        self.assertEqual(state.get("in_progress_window_start"), to_iso(backfill))

    def test_first_run_without_a_backfill_point_looks_back(self):
        state = FakeState()
        watermark = Watermark(state, lookback_days=30, backfill_from=None)
        start, _, _ = watermark.begin(NOW)
        self.assertEqual(start, NOW - timedelta(days=30))

    def test_later_runs_rescan_the_overlap(self):
        committed = datetime(2026, 9, 12, 11, 0, tzinfo=UTC)
        state = FakeState({"sync_cursor": to_iso(committed)})
        watermark = Watermark(state, overlap_seconds=600)

        start, cursor, resumed = watermark.begin(NOW)

        self.assertEqual(start, committed - timedelta(seconds=600))
        self.assertIsNone(cursor)
        self.assertFalse(resumed)

    def test_commit_advances_from_the_run_start_not_its_end(self):
        state = FakeState()
        watermark = Watermark(state, overlap_seconds=600, backfill_from=NOW)
        watermark.begin(NOW)

        watermark.commit(NOW)

        self.assertEqual(parse_iso(state.get("sync_cursor")), NOW - timedelta(seconds=600))
        # The window is closed, so the next run opens a fresh one.
        self.assertIsNone(state.get("in_progress_cursor"))
        self.assertIsNone(state.get("in_progress_window_start"))

    def test_an_interrupted_window_resumes_from_its_cursor(self):
        backfill = datetime(2025, 1, 1, tzinfo=UTC)
        state = FakeState()
        first = Watermark(state, overlap_seconds=600, backfill_from=backfill)
        first.begin(NOW)
        first.save_cursor("page-2")

        # A separate instance stands in for the next run.
        start, cursor, resumed = Watermark(state, overlap_seconds=600).begin(NOW)

        self.assertTrue(resumed)
        self.assertEqual(cursor, "page-2")
        self.assertEqual(start, backfill, "must resume the same window, not a new one")

    def test_a_window_with_no_cursor_reopens(self):
        # Crashed after opening the window but before finishing a page.
        state = FakeState({"in_progress_window_start": to_iso(datetime(2025, 1, 1, tzinfo=UTC))})
        watermark = Watermark(state, backfill_from=datetime(2025, 6, 1, tzinfo=UTC))

        start, cursor, resumed = watermark.begin(NOW)

        self.assertFalse(resumed)
        self.assertIsNone(cursor)
        self.assertEqual(start, datetime(2025, 6, 1, tzinfo=UTC))

    def test_save_cursor_ignores_empty_values(self):
        state = FakeState()
        watermark = Watermark(state)
        watermark.begin(NOW)
        watermark.save_cursor(None)
        watermark.save_cursor("")
        self.assertIsNone(state.get("in_progress_cursor"))

    def test_reset_clears_everything(self):
        state = FakeState()
        watermark = Watermark(state, backfill_from=NOW)
        watermark.begin(NOW)
        watermark.save_cursor("page-1")

        watermark.reset()

        self.assertIsNone(state.get("sync_cursor"))
        self.assertIsNone(state.get("in_progress_window_start"))
        self.assertIsNone(state.get("in_progress_cursor"))


# --------------------------------------------------------------------------- #
# Salesforce helpers
# --------------------------------------------------------------------------- #


class PickAllowedTests(unittest.TestCase):
    ALLOWED = {"united states", "canada"}

    def test_prefers_the_primary_when_the_org_accepts_it(self):
        self.assertEqual(
            pick_allowed("US", "United States", {"us", "united states"}), "US")

    def test_falls_back_to_the_alternate_when_only_that_matches(self):
        # The org's picklist uses full names, so the ISO code must not be sent.
        self.assertEqual(pick_allowed("US", "United States", self.ALLOWED), "United States")

    def test_omits_the_field_when_neither_matches(self):
        self.assertEqual(pick_allowed("XX", "Narnia", self.ALLOWED), "")

    def test_passes_values_through_when_the_field_is_not_a_picklist(self):
        self.assertEqual(pick_allowed("US", "United States", None), "US")
        self.assertEqual(pick_allowed("", "United States", None), "United States")
        self.assertEqual(pick_allowed(None, None, None), "")

    def test_matches_the_candidate_case_insensitively_and_returns_it_verbatim(self):
        # `allowed` is lowercased by picklist_values(), so callers pass it that
        # way; the candidate is lowercased for comparison and returned as given.
        self.assertEqual(pick_allowed("US", None, {"us"}), "US")
        self.assertEqual(pick_allowed("us", None, {"us"}), "us")
        self.assertEqual(pick_allowed("United States", None, {"united states"}), "United States")


class AddressPicklistErrorTests(unittest.TestCase):
    def test_recognises_salesforces_wording(self):
        message = ("HTTP 400: INVALID_OR_NULL_FOR_RESTRICTED_PICKLIST: There's a problem "
                   "with this country, even though it may appear correct. Please select a "
                   "country/territory from the list of valid countries.: Mailing Country")
        self.assertTrue(is_address_picklist_error(message))

    def test_recognises_the_free_text_wording_without_a_code(self):
        self.assertTrue(is_address_picklist_error(
            "There's a problem with this country, even though it may appear correct."))

    def test_ignores_unrelated_failures(self):
        for message in ("HTTP 400: REQUIRED_FIELD_MISSING: Required fields are missing: [AccountId]",
                        "HTTP 400: DUPLICATES_DETECTED: Duplicate rule rejected this record",
                        ""):
            self.assertFalse(is_address_picklist_error(message), message)


class CombineErrorsTests(unittest.TestCase):
    def test_reports_the_per_record_reason(self):
        self.assertEqual(combine_errors("per-record", ""), "per-record")

    def test_keeps_the_batch_reason_when_it_differs(self):
        self.assertEqual(combine_errors("per-record", "batch"), "per-record (batch: batch)")

    def test_falls_back_to_the_batch_reason(self):
        self.assertEqual(combine_errors("", "batch"), "batch")

    def test_does_not_repeat_an_identical_reason(self):
        self.assertEqual(combine_errors("HTTP 400: same", "same"), "HTTP 400: same")


class RequestShapeTests(unittest.TestCase):
    """The shapes Salesforce actually accepts.

    An earlier version used POST for the collections endpoint, put allOrNone in
    the body, and sent the external ID in the single-record body; a mock that
    accepted all three hid the bugs, so these assert them explicitly.
    """

    def setUp(self):
        self.seen = []
        seen = self.seen

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args):
                pass

            def _send(self, code, payload):
                raw = json.dumps(payload).encode()
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def do_POST(self):
                length = int(self.headers.get("Content-Length") or 0)
                self.rfile.read(length)
                if self.path.endswith("/services/oauth2/token"):
                    self._send(200, {"access_token": "t",
                                     "instance_url": "http://127.0.0.1:%d" % self.server.server_address[1]})
                    return
                seen.append(("POST", self.path, None))
                self._send(200, [])

            def do_PATCH(self):
                length = int(self.headers.get("Content-Length") or 0)
                raw = self.rfile.read(length)
                body = json.loads(raw) if raw else None
                seen.append(("PATCH", self.path, body))
                if "/composite/sobjects/" in self.path:
                    self._send(200, [{"id": "003", "success": True, "created": True, "errors": []}
                                     for _ in (body or {}).get("records") or []])
                else:
                    self._send(201, {"id": "003", "success": True})

        self.server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.base = "http://127.0.0.1:%d" % self.server.server_address[1]

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()

    def _client(self, batch_size):
        return SalesforceClient(self.base, "62.0", "client_credentials", "id", "secret",
                                batch_size=batch_size)

    def test_batched_upsert_uses_patch_with_a_query_parameter(self):
        records = [{"Shopify_Customer_Id__c": "1", "LastName": "One"},
                   {"Shopify_Customer_Id__c": "2", "LastName": "Two"}]

        written, failures = self._client(batch_size=200).upsert(
            "Contact", "Shopify_Customer_Id__c", records)

        self.assertEqual((written, failures), (2, []))
        method, path, body = self.seen[-1]
        self.assertEqual(method, "PATCH")
        self.assertIn("?allOrNone=false", path)
        self.assertTrue(path.startswith(
            "/services/data/v62.0/composite/sobjects/Contact/Shopify_Customer_Id__c"))
        self.assertNotIn("allOrNone", body, "allOrNone must be a query parameter")
        for record in body["records"]:
            self.assertEqual(record["attributes"]["type"], "Contact")
            self.assertIn("Shopify_Customer_Id__c", record)

    def test_single_upsert_keeps_the_external_id_out_of_the_body(self):
        written, failures = self._client(batch_size=1).upsert(
            "Contact", "Shopify_Customer_Id__c",
            [{"Shopify_Customer_Id__c": "1", "LastName": "One", "Email": "a@b.c"}])

        self.assertEqual((written, failures), (1, []))
        method, path, body = self.seen[-1]
        self.assertEqual(method, "PATCH")
        self.assertTrue(path.endswith("/sobjects/Contact/Shopify_Customer_Id__c/1"))
        self.assertNotIn("Shopify_Customer_Id__c", body)
        self.assertEqual(body["LastName"], "One")


# --------------------------------------------------------------------------- #
# Smaller helpers
# --------------------------------------------------------------------------- #


class TimeUtilTests(unittest.TestCase):
    def test_round_trip(self):
        self.assertEqual(parse_iso(to_iso(NOW)), NOW)

    def test_tolerates_junk_and_empties(self):
        for value in (None, "", "not a date", 12345):
            self.assertIsNone(parse_iso(value), value)

    def test_accepts_the_forms_shopify_and_salesforce_return(self):
        self.assertEqual(parse_iso("2026-01-01T00:00:00Z"), datetime(2026, 1, 1, tzinfo=UTC))
        self.assertEqual(parse_iso("2026-01-01T00:00:00+00:00"), datetime(2026, 1, 1, tzinfo=UTC))

    def test_utcnow_is_timezone_aware(self):
        self.assertIsNotNone(utcnow().tzinfo)


class ConfigTests(unittest.TestCase):
    def setUp(self):
        self.saved = dict(os.environ)

    def tearDown(self):
        os.environ.clear()
        os.environ.update(self.saved)

    def test_defaults_and_stripping(self):
        os.environ.pop("OTTER_TEST_VALUE", None)
        self.assertEqual(env("OTTER_TEST_VALUE", "fallback"), "fallback")
        os.environ["OTTER_TEST_VALUE"] = "  spaced  "
        self.assertEqual(env("OTTER_TEST_VALUE"), "spaced")
        os.environ["OTTER_TEST_VALUE"] = "   "
        self.assertEqual(env("OTTER_TEST_VALUE", "fallback"), "fallback")

    def test_integers(self):
        os.environ["OTTER_TEST_INT"] = "42"
        self.assertEqual(env_int("OTTER_TEST_INT", 1), 42)
        os.environ["OTTER_TEST_INT"] = "nope"
        with self.assertRaises(ConfigError):
            env_int("OTTER_TEST_INT", 1)

    def test_booleans(self):
        for raw in ("1", "true", "TRUE", "yes", "on"):
            os.environ["OTTER_TEST_BOOL"] = raw
            self.assertTrue(env_bool("OTTER_TEST_BOOL"), raw)
        for raw in ("0", "false", "no", "off", ""):
            os.environ["OTTER_TEST_BOOL"] = raw
            self.assertFalse(env_bool("OTTER_TEST_BOOL"), raw)
        os.environ.pop("OTTER_TEST_BOOL", None)
        self.assertTrue(env_bool("OTTER_TEST_BOOL", True))

    def test_required(self):
        os.environ.pop("OTTER_TEST_REQUIRED", None)
        with self.assertRaises(ConfigError):
            require_env("OTTER_TEST_REQUIRED")
        os.environ["OTTER_TEST_REQUIRED"] = "value"
        self.assertEqual(require_env("OTTER_TEST_REQUIRED"), "value")


if __name__ == "__main__":
    unittest.main()
