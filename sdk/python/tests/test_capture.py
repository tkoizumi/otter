"""Tests for the SDK's bounded HTTP capture.

Run with::

    python3 -m unittest discover -s sdk/python/tests

Everything here is local: a fake daemon receives the capture batches and a fake
origin server provides the requests being inspected. No third-party packages and
no network access are needed.

The redaction corpus is shared with the Go side
(``testdata/capture/redaction_fixtures.json``) so the two implementations cannot
drift apart.
"""

import io
import json
import os
import socket
import sys
import threading
import time
import unittest
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
SDK_ROOT = os.path.dirname(HERE)
REPO_ROOT = os.path.dirname(os.path.dirname(SDK_ROOT))
if SDK_ROOT not in sys.path:
    sys.path.insert(0, SDK_ROOT)

from otter import _capture  # noqa: E402
from otter._client import Client  # noqa: E402

FIXTURES = os.path.join(REPO_ROOT, "testdata", "capture", "redaction_fixtures.json")

#: A distinctive value that must never appear in stored capture.
SECRET_MARKER = "sup3r-s3cret-value"


# --------------------------------------------------------------- fake servers

class _CaptureSink(BaseHTTPRequestHandler):
    """A stand-in for the daemon's capture ingestion endpoint."""

    batches = []
    lock = threading.Lock()

    def do_POST(self):  # noqa: N802 - http.server naming
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length)
        try:
            payload = json.loads(raw.decode("utf-8"))
        except ValueError:
            payload = {"_undecodable": raw.decode("utf-8", "replace")}
        with type(self).lock:
            type(self).batches.append({
                "path": self.path,
                "authorization": self.headers.get("Authorization"),
                "payload": payload,
            })
        self._respond(202, {"accepted": True})

    def do_GET(self):  # noqa: N802 - http.server naming
        self._respond(200, {"ok": True})

    def _respond(self, status, payload):
        body = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):  # keep the test output clean
        pass

    def handle_error(self, *args):  # a client timing out mid-response is expected
        pass


class _OriginHandler(BaseHTTPRequestHandler):
    """A stand-in for the API an integration calls."""

    def do_GET(self):  # noqa: N802
        if self.path.startswith("/redirect"):
            self.send_response(302)
            self.send_header("Location", "/json")
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        if self.path.startswith("/slow"):
            time.sleep(0.6)
            self._json(200, {"slow": True})
            return
        if self.path.startswith("/stream"):
            self._json(200, {"a": 1, "b": 2})
            return
        if self.path.startswith("/text"):
            self._body(200, b"plain text", "text/plain")
            return
        if self.path.startswith("/gzipped"):
            # A body the client cannot decode; urllib does not decompress.
            self._body(200, b"\x1f\x8b\x08\x00", "application/json", encoding="gzip")
            return
        self._json(200, {"ok": True, "password": SECRET_MARKER})

    def do_POST(self):  # noqa: N802
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length)
        if self.path.startswith("/bad"):
            self._json(400, {"error": "bad request", "token": SECRET_MARKER})
            return
        try:
            parsed = json.loads(raw.decode("utf-8"))
        except ValueError:
            parsed = None
        self._json(200, {"received": parsed})

    def _json(self, status, payload):
        self._body(status, json.dumps(payload).encode("utf-8"), "application/json")

    def _body(self, status, body, content_type, encoding=None):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        if encoding:
            self.send_header("Content-Encoding", encoding)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass

    def handle_error(self, *args):  # a client timing out mid-response is expected
        pass


def _start(handler):
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, "http://127.0.0.1:%d" % server.server_address[1]


# ----------------------------------------------------------------- base case

class CaptureTestCase(unittest.TestCase):
    def setUp(self):
        with _CaptureSink.lock:
            _CaptureSink.batches = []
        self.sink, self.sink_url = _start(_CaptureSink)
        self.origin, self.origin_url = _start(_OriginHandler)
        self.addCleanup(self.sink.shutdown)
        self.addCleanup(self.origin.shutdown)

    # -- helpers --------------------------------------------------------

    def start_capture(self, policy="full", **kwargs):
        capture = _capture.Capture(
            run_id="run-1", api_url=self.sink_url, token="tok", policy=policy, **kwargs
        )
        capture.start()
        self.addCleanup(capture.shutdown)
        return capture

    def batches(self):
        with _CaptureSink.lock:
            return list(_CaptureSink.batches)

    def events(self):
        out = []
        for batch in self.batches():
            out.extend(batch["payload"].get("events") or [])
        return out

    def find_event(self, kind):
        for event in self.events():
            if event.get("kind") == kind:
                return event
        return None


# ------------------------------------------------------------------ redaction

class RedactionTests(CaptureTestCase):
    """The shared corpus, asserted identically by the Go implementation."""

    @classmethod
    def setUpClass(cls):
        with open(FIXTURES, "r", encoding="utf-8") as handle:
            cls.fixtures = json.load(handle)

    def setUp(self):
        super().setUp()
        self.redactor = _capture.Redactor()

    def test_urls(self):
        for case in self.fixtures["urls"]:
            with self.subTest(case["name"]):
                got, count = self.redactor.sanitize_url(case["input"])
                self.assertEqual(case["expect"], got)
                self.assertEqual(case["redactions"], count)

    def test_headers(self):
        for case in self.fixtures["headers"]:
            with self.subTest(case["name"]):
                pairs = [(row[0], row[1]) for row in case["input"]]
                got, count = self.redactor.sanitize_headers(pairs)
                self.assertEqual(
                    [{"name": row[0], "value": row[1]} for row in case["expect"]], got
                )
                self.assertEqual(case["redactions"], count)

    def test_json(self):
        for case in self.fixtures["json"]:
            with self.subTest(case["name"]):
                got, count = self.redactor.sanitize_json(case["input"].encode("utf-8"))
                # Exact text, not a re-encoded comparison: key order and number
                # literals are part of what this corpus pins down.
                self.assertEqual(case["expect"], got.decode("utf-8"))
                self.assertEqual(case["redactions"], count)

    def test_invalid_json_is_rejected(self):
        for case in self.fixtures["invalid_json"]:
            with self.subTest(case["name"]):
                with self.assertRaises(ValueError):
                    self.redactor.sanitize_json(case["input"].encode("utf-8"))

    def test_error_text(self):
        for case in self.fixtures["error_text"]:
            with self.subTest(case["name"]):
                got = self.redactor.sanitize_error_text(case["input"])
                for wanted in case["expect_contains"]:
                    self.assertIn(wanted, got)
                for unwanted in case["expect_not_contains"]:
                    self.assertNotIn(unwanted, got)

    def test_operator_rules_cannot_weaken_defaults(self):
        redactor = _capture.Redactor(extra_headers=["X-Tenant-Secret"])
        pairs, count = redactor.sanitize_headers([
            ("X-Tenant-Secret", "v"), ("Authorization", "v"),
        ])
        self.assertEqual(2, count)
        self.assertEqual(_capture.REDACTED, pairs[0]["value"])
        self.assertEqual(_capture.REDACTED, pairs[1]["value"])


# ------------------------------------------------------------ instrumentation

class InstrumentationTests(CaptureTestCase):
    def test_get_is_captured_and_the_caller_result_is_unchanged(self):
        # Without capture: the baseline the integration should keep seeing.
        with urllib.request.urlopen(self.origin_url + "/json") as response:
            baseline = (response.status, json.loads(response.read().decode("utf-8")))

        capture = self.start_capture()
        with urllib.request.urlopen(self.origin_url + "/json") as response:
            observed = (response.status, json.loads(response.read().decode("utf-8")))

        self.assertEqual(baseline, observed)

        # Capture is delivered in the background; flushing makes the assertion
        # deterministic rather than racing the worker.
        capture.shutdown()

        started = self.find_event("request.started")
        self.assertIsNotNone(started)
        self.assertEqual("GET", started["method"])
        self.assertEqual(self.origin_url + "/json", started["url"])
        self.assertTrue(started.get("call_site"))

        completed = self.find_event("request.completed")
        self.assertIsNotNone(completed)
        self.assertEqual(200, completed["status_code"])
        self.assertIsNone(completed.get("transport_error"))
        self.assertEqual("captured", completed["response_body"]["state"])
        # A credential-shaped field must not survive.
        self.assertEqual(
            _capture.REDACTED, completed["response_body"]["json"]["password"]
        )
        self.assertNotIn(SECRET_MARKER, json.dumps(self.batches()))

    def test_json_post_request_body_is_captured_and_redacted(self):
        capture = self.start_capture()
        payload = {"name": "ok", "password": SECRET_MARKER, "nested": {"token": SECRET_MARKER}}
        request = urllib.request.Request(
            self.origin_url + "/json",
            data=json.dumps(payload).encode("utf-8"),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(request) as response:
            response.read()
        capture.shutdown()

        started = self.find_event("request.started")
        self.assertEqual("POST", started["method"])
        body = started["request_body"]
        self.assertEqual("captured", body["state"])
        self.assertEqual(_capture.REDACTED, body["json"]["password"])
        self.assertEqual(_capture.REDACTED, body["json"]["nested"]["token"])
        self.assertTrue(body["redacted"])
        self.assertNotIn(SECRET_MARKER, json.dumps(self.batches()))

    def test_http_400_body_is_readable_and_captured(self):
        """The acceptance scenario at the SDK boundary."""
        capture = self.start_capture()
        request = urllib.request.Request(
            self.origin_url + "/bad",
            data=json.dumps({"customer_id": 123}).encode("utf-8"),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with self.assertRaises(urllib.error.HTTPError) as raised:
            urllib.request.urlopen(request)
        # The caller's error body must still be readable exactly once.
        error_body = json.loads(raised.exception.read().decode("utf-8"))
        self.assertEqual("bad request", error_body["error"])
        self.assertEqual(SECRET_MARKER, error_body["token"])
        capture.shutdown()

        completed = self.find_event("request.completed")
        self.assertIsNotNone(completed)
        self.assertEqual(400, completed["status_code"])
        self.assertEqual("captured", completed["response_body"]["state"])
        self.assertEqual("bad request", completed["response_body"]["json"]["error"])
        # The captured copy is redacted even though the caller's is not.
        self.assertEqual(
            _capture.REDACTED, completed["response_body"]["json"]["token"]
        )
        self.assertNotIn(SECRET_MARKER, json.dumps(self.batches()))

    def test_transport_error_is_captured_and_original_exception_propagates(self):
        capture = self.start_capture()
        with self.assertRaises((urllib.error.URLError, OSError)) as raised:
            urllib.request.urlopen("http://127.0.0.1:1/nothing", timeout=1.0)
        capture.shutdown()

        self.assertTrue(raised.exception)
        completed = self.find_event("request.completed")
        self.assertIsNotNone(completed)
        self.assertTrue(completed.get("transport_error_class"))
        self.assertIsNone(completed.get("status_code"))

    def test_timeout_is_captured_and_still_raises(self):
        capture = self.start_capture()
        with self.assertRaises((urllib.error.URLError, OSError, socket.timeout)):
            urllib.request.urlopen(self.origin_url + "/slow", timeout=0.2)
        capture.shutdown()

        completed = self.find_event("request.completed")
        self.assertIsNotNone(completed)
        self.assertTrue(completed.get("transport_error_class"))

    def test_redirect_is_one_exchange_with_a_final_url(self):
        capture = self.start_capture()
        with urllib.request.urlopen(self.origin_url + "/redirect") as response:
            response.read()
            final_url = response.geturl()
        capture.shutdown()

        self.assertTrue(final_url.endswith("/json"))
        started_events = [e for e in self.events() if e["kind"] == "request.started"]
        self.assertEqual(1, len(started_events), "a redirect must not invent a second request")
        completed = self.find_event("request.completed")
        self.assertTrue(completed.get("final_url", "").endswith("/json"))

    def test_partial_read_is_marked_incomplete_not_truncated(self):
        capture = self.start_capture()
        with urllib.request.urlopen(self.origin_url + "/stream") as response:
            first = response.read(4)
        self.assertTrue(first)
        capture.shutdown()

        completed = self.find_event("request.completed")
        body = completed["response_body"]
        self.assertEqual("omitted", body["state"])
        self.assertEqual("incomplete", body["reason"])
        self.assertNotIn("json", body)

    def test_close_without_reading_omits_the_body(self):
        capture = self.start_capture()
        response = urllib.request.urlopen(self.origin_url + "/json")
        status = response.status
        response.close()
        self.assertEqual(200, status)
        capture.shutdown()

        completed = self.find_event("request.completed")
        self.assertEqual("omitted", completed["response_body"]["state"])
        self.assertEqual("incomplete", completed["response_body"]["reason"])

    def test_iteration_and_readinto_are_preserved(self):
        capture = self.start_capture()
        with urllib.request.urlopen(self.origin_url + "/json") as response:
            buffer = bytearray(8)
            read = response.readinto(buffer)
            lines = list(response)
        self.assertTrue(read and read > 0)
        self.assertIsInstance(lines, list)
        capture.shutdown()
        self.assertIsNotNone(self.find_event("request.completed"))

    def test_unsupported_content_type_is_reported(self):
        capture = self.start_capture()
        with urllib.request.urlopen(self.origin_url + "/text") as response:
            response.read()
        capture.shutdown()

        body = self.find_event("request.completed")["response_body"]
        self.assertEqual("omitted", body["state"])
        self.assertEqual("unsupported_content", body["reason"])

    def test_content_encoded_body_is_reported_not_parsed(self):
        capture = self.start_capture()
        with urllib.request.urlopen(self.origin_url + "/gzipped") as response:
            response.read()
        capture.shutdown()

        body = self.find_event("request.completed")["response_body"]
        self.assertEqual("omitted", body["state"])
        self.assertEqual("encoded", body["reason"])

    def test_streamed_request_body_is_omitted(self):
        capture = self.start_capture()

        def chunks():
            yield b"a"
            yield b"b"

        capture.begin("POST", self.origin_url + "/json", [], chunks())
        capture.shutdown()

        started = self.find_event("request.started")
        self.assertEqual("omitted", started["request_body"]["state"])
        self.assertEqual("stream_unsupported", started["request_body"]["reason"])

    def test_metadata_policy_records_no_payloads(self):
        capture = self.start_capture(policy="metadata")
        with urllib.request.urlopen(self.origin_url + "/json") as response:
            response.read()
        capture.shutdown()

        started = self.find_event("request.started")
        self.assertNotIn("request_headers", started)
        self.assertNotIn("request_body", started)
        completed = self.find_event("request.completed")
        self.assertIsNone(completed.get("response_body"))
        self.assertEqual(200, completed["status_code"])

    def test_two_concurrent_requests_are_both_captured(self):
        capture = self.start_capture()
        results = []
        lock = threading.Lock()

        def fetch():
            with urllib.request.urlopen(self.origin_url + "/json") as response:
                data = response.read()
            with lock:
                results.append(data)

        threads = [threading.Thread(target=fetch) for _ in range(2)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()
        capture.shutdown()

        self.assertEqual(2, len(results))
        starts = [e for e in self.events() if e["kind"] == "request.started"]
        self.assertEqual(2, len(starts))
        self.assertEqual(2, len({e["request_id"] for e in starts}))

    def test_error_body_is_identical_with_capture_off_and_on(self):
        """Capture must not alter what the caller sees on an error path."""

        def fetch():
            request = urllib.request.Request(
                self.origin_url + "/bad",
                data=json.dumps({"customer_id": 123}).encode("utf-8"),
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            try:
                urllib.request.urlopen(request)
                return None
            except urllib.error.HTTPError as exc:
                return exc.code, exc.read()

        baseline = fetch()
        self.assertIsNotNone(baseline)
        self.assertIn(b"bad request", baseline[1])

        capture = self.start_capture()
        observed = fetch()
        capture.shutdown()

        self.assertEqual(baseline, observed)

    def test_custom_opener_is_captured(self):
        capture = self.start_capture()
        opener = urllib.request.build_opener()
        with opener.open(self.origin_url + "/json") as response:
            response.read()
        capture.shutdown()

        started = self.find_event("request.started")
        self.assertIsNotNone(started, "a custom opener using the base implementation must be covered")
        self.assertEqual(self.origin_url + "/json", started["url"])

    def test_delivery_outage_does_not_fail_the_integration(self):
        # Nothing is listening on this port, so capture cannot be delivered.
        capture = _capture.Capture(
            run_id="run-1", api_url="http://127.0.0.1:1", token="tok", policy="full"
        )
        capture.start()
        try:
            with urllib.request.urlopen(self.origin_url + "/json") as response:
                body = response.read()
            self.assertTrue(body, "the integration's own request must still succeed")
        finally:
            capture.shutdown()

    def test_otter_control_traffic_is_never_captured(self):
        capture = self.start_capture()
        client = Client(self.sink_url, token="tok")
        client.get_json("/v1/health")
        capture.shutdown()

        for event in self.events():
            if event["kind"] == "request.started":
                self.assertNotIn("/v1/health", event.get("url", ""))

    def test_schema_version_and_policy_travel_with_every_batch(self):
        capture = self.start_capture()
        with urllib.request.urlopen(self.origin_url + "/json") as response:
            response.read()
        capture.shutdown()

        delivered = self.batches()
        self.assertTrue(delivered)
        for batch in delivered:
            self.assertEqual(_capture.SCHEMA_VERSION, batch["payload"]["schema_version"])
            self.assertEqual("full", batch["payload"]["policy"])
            self.assertEqual("Bearer tok", batch["authorization"])
            self.assertTrue(batch["path"].startswith("/v1/runs/run-1/requests/events"))

    def test_shutdown_reports_finalization(self):
        capture = self.start_capture()
        with urllib.request.urlopen(self.origin_url + "/json") as response:
            response.read()
        capture.shutdown()

        summaries = [
            batch["payload"] for batch in self.batches()
            if batch["payload"].get("finalization")
        ]
        self.assertTrue(summaries, "shutdown must report a finalization")
        self.assertEqual("complete", summaries[-1]["finalization"])
        self.assertEqual(0, summaries[-1]["dropped_events"])

    def test_queue_overflow_drops_capture_and_says_so(self):
        capture = self.start_capture(queue_max=1)
        # Two completed exchanges with a one-slot queue: the first is dropped.
        capture.begin("GET", "https://example.test/one")
        capture.begin("GET", "https://example.test/two")
        capture.shutdown()

        summaries = [
            batch["payload"] for batch in self.batches()
            if batch["payload"].get("finalization")
        ]
        self.assertTrue(summaries)
        self.assertEqual("incomplete", summaries[-1]["finalization"])
        self.assertGreaterEqual(summaries[-1]["dropped_events"], 1)


class InstallationTests(CaptureTestCase):
    def test_capture_is_off_without_an_explicit_policy(self):
        for policy in (None, "", "off", "everything"):
            env = {
                "OTTER_RUN_ID": "run-1",
                "OTTER_API_URL": self.sink_url,
                "OTTER_STATE_TOKEN": "tok",
            }
            if policy is not None:
                env["OTTER_CAPTURE_POLICY"] = policy
            self.assertIsNone(_capture.install_from_environment(env))

    def test_install_from_environment_starts_and_flushes(self):
        env = {
            "OTTER_RUN_ID": "run-env",
            "OTTER_API_URL": self.sink_url,
            "OTTER_STATE_TOKEN": "tok",
            "OTTER_CAPTURE_POLICY": "full",
        }
        capture = _capture.install_from_environment(env)
        self.assertIsNotNone(capture)
        self.addCleanup(capture.shutdown)
        with urllib.request.urlopen(self.origin_url + "/json") as response:
            response.read()
        _capture.flush_before_exit()
        # Idempotent: the launcher may flush again on the way out.
        _capture.flush_before_exit()

        self.assertTrue(any(
            batch["path"].startswith("/v1/runs/run-env/") for batch in self.batches()
        ))

    def test_uninstall_restores_the_original_opener(self):
        original = urllib.request.OpenerDirector.open
        capture = self.start_capture()
        self.assertNotEqual(original, urllib.request.OpenerDirector.open)
        capture.shutdown()
        self.assertEqual(original, urllib.request.OpenerDirector.open)


if __name__ == "__main__":
    unittest.main()
