"""Tests for the optional transport adapters: requests and httpx.

These run only when the library under test is importable, because the SDK never
depends on either. The fake daemon and origin servers come from ``test_capture``;
nothing here talks to the network.

Run with::

    python3 -m unittest discover -s sdk/python/tests

To exercise the requests half where requests is not the system interpreter,
install it into a throwaway environment and run the same discovery with that
interpreter.
"""

import asyncio
import json
import os
import socket
import sys
import threading
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SDK_ROOT = os.path.dirname(HERE)
if SDK_ROOT not in sys.path:
    sys.path.insert(0, SDK_ROOT)
if HERE not in sys.path:
    sys.path.insert(0, HERE)

from otter import _capture  # noqa: E402
from test_capture import SECRET_MARKER, CaptureTestCase  # noqa: E402

try:
    import requests
except ImportError:  # pragma: no cover - depends on the interpreter
    requests = None

try:
    import httpx
except ImportError:  # pragma: no cover - depends on the interpreter
    httpx = None


def _closed_port():
    """Reserve and release a loopback port, so connecting to it is refused."""
    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    port = sock.getsockname()[1]
    sock.close()
    return port


class _AdapterAssertions(CaptureTestCase):
    """Helpers shared by the requests and httpx suites."""

    def start_capture(self, policy="full", **kwargs):
        capture = super().start_capture(policy, **kwargs)
        self._adapter_capture = capture
        return capture

    def events(self):
        # Capture is delivered in the background, so reading events flushes it
        # first: assertions are then deterministic rather than racing the worker.
        # shutdown is idempotent, and every test makes its requests before it
        # reads anything back.
        capture = getattr(self, "_adapter_capture", None)
        if capture is not None:
            capture.shutdown()
        return super().events()

    def completed_events(self):
        return [e for e in self.events() if e.get("kind") == "request.completed"]

    def assert_payloads_are_sanitized(self):
        for event in self.events():
            self.assertNotIn(SECRET_MARKER, json.dumps(event))


# ------------------------------------------------------------------- requests

@unittest.skipUnless(requests is not None, "requests is not installed")
class RequestsAdapterTests(_AdapterAssertions):
    def test_the_recording_names_the_requests_adapter(self):
        capture = self.start_capture()
        self.assertIn(_capture.ADAPTER_REQUESTS, capture.adapters)

    def test_get_is_captured_without_changing_the_result(self):
        baseline = requests.get(self.origin_url + "/json")
        self.start_capture()

        response = requests.get(self.origin_url + "/json")

        self.assertEqual(baseline.status_code, response.status_code)
        self.assertEqual(baseline.json(), response.json())

        start = self.find_event("request.started")
        self.assertEqual("GET", start["method"])
        self.assertEqual(self.origin_url + "/json", start["url"])
        self.assertIn("test_adapters.py", start["call_site"])

        completed = self.find_event("request.completed")
        self.assertEqual(200, completed["status_code"])
        self.assertEqual("captured", completed["response_body"]["state"])

    def test_post_body_and_error_response_are_captured_and_redacted(self):
        self.start_capture()

        response = requests.post(
            self.origin_url + "/bad", json={"customer_id": 123, "token": SECRET_MARKER}
        )

        self.assertEqual(400, response.status_code)
        start = self.find_event("request.started")
        self.assertEqual({"customer_id": 123, "token": "REDACTED"},
                         start["request_body"]["json"])
        completed = self.find_event("request.completed")
        self.assertEqual(400, completed["status_code"])
        self.assertEqual("REDACTED", completed["response_body"]["json"]["token"])
        self.assert_payloads_are_sanitized()

    def test_streamed_body_is_observed_as_it_is_consumed(self):
        self.start_capture()

        with requests.get(self.origin_url + "/json", stream=True) as response:
            body = response.content

        self.assertEqual(200, response.status_code)
        self.assertIn(b"ok", body)
        completed = self.find_event("request.completed")
        self.assertEqual("captured", completed["response_body"]["state"])
        self.assertEqual(len(body), completed["response_body"]["bytes_observed"])

    def test_a_partial_stream_is_reported_incomplete_not_invented(self):
        self.start_capture()

        response = requests.get(self.origin_url + "/json", stream=True)
        iterator = response.iter_content(4)
        next(iterator)
        response.close()
        iterator.close()

        completed = self.find_event("request.completed")
        self.assertEqual("omitted", completed["response_body"]["state"])
        self.assertEqual("incomplete", completed["response_body"]["reason"])
        self.assertNotIn("json", completed["response_body"])

    def test_a_transport_error_is_recorded_and_reraised(self):
        self.start_capture()
        port = _closed_port()

        with self.assertRaises(requests.exceptions.ConnectionError):
            requests.get("http://127.0.0.1:%d/" % port, timeout=1)

        completed = self.find_event("request.completed")
        self.assertEqual("ConnectionError", completed["transport_error_class"])
        self.assertTrue(completed["transport_error"])

    def test_a_redirect_is_one_exchange_with_a_final_url(self):
        self.start_capture()

        response = requests.get(self.origin_url + "/redirect")

        self.assertEqual(200, response.status_code)
        self.assertEqual(1, len(self.completed_events()))
        start = self.find_event("request.started")
        self.assertTrue(start["url"].endswith("/redirect"))
        completed = self.find_event("request.completed")
        self.assertEqual(self.origin_url + "/json", completed["final_url"])

    def test_suppressed_requests_are_not_recorded(self):
        self.start_capture()

        with _capture.suppressed():
            requests.get(self.origin_url + "/json")

        self.assertEqual([], self.events())

    def test_metadata_policy_stores_no_headers_or_bodies(self):
        self.start_capture(policy="metadata")

        requests.post(self.origin_url + "/bad", json={"token": SECRET_MARKER})

        start = self.find_event("request.started")
        self.assertIsNone(start.get("request_headers"))
        self.assertIsNone(start.get("request_body"))
        completed = self.find_event("request.completed")
        self.assertEqual(400, completed["status_code"])
        self.assertIsNone(completed.get("response_body"))
        self.assert_payloads_are_sanitized()

    def test_two_threads_produce_two_records(self):
        self.start_capture()
        errors = []

        def call():
            try:
                requests.get(self.origin_url + "/json", timeout=5)
            except BaseException as exc:  # recorded below, never swallowed
                errors.append(exc)

        threads = [threading.Thread(target=call) for _ in range(2)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()

        self.assertEqual([], errors)
        self.assertEqual(2, len(self.completed_events()))


# ---------------------------------------------------------------------- httpx

@unittest.skipUnless(httpx is not None, "httpx is not installed")
class HttpxAdapterTests(_AdapterAssertions):
    def test_the_recording_names_the_httpx_adapter(self):
        capture = self.start_capture()
        self.assertIn(_capture.ADAPTER_HTTPX, capture.adapters)

    def test_get_is_captured_without_changing_the_result(self):
        baseline = httpx.get(self.origin_url + "/json")
        self.start_capture()

        response = httpx.get(self.origin_url + "/json")

        self.assertEqual(baseline.status_code, response.status_code)
        self.assertEqual(baseline.json(), response.json())

        start = self.find_event("request.started")
        self.assertEqual("GET", start["method"])
        self.assertEqual(self.origin_url + "/json", start["url"])
        self.assertIn("test_adapters.py", start["call_site"])

        completed = self.find_event("request.completed")
        self.assertEqual(200, completed["status_code"])
        self.assertEqual("captured", completed["response_body"]["state"])

    def test_post_body_and_error_response_are_captured_and_redacted(self):
        self.start_capture()

        response = httpx.post(
            self.origin_url + "/bad", json={"customer_id": 123, "access_token": SECRET_MARKER}
        )

        self.assertEqual(400, response.status_code)
        start = self.find_event("request.started")
        self.assertEqual({"customer_id": 123, "access_token": "REDACTED"},
                         start["request_body"]["json"])
        completed = self.find_event("request.completed")
        self.assertEqual(400, completed["status_code"])
        self.assertEqual("REDACTED", completed["response_body"]["json"]["token"])
        self.assert_payloads_are_sanitized()

    def test_streamed_body_is_observed_as_it_is_consumed(self):
        self.start_capture()

        with httpx.stream("GET", self.origin_url + "/json") as response:
            body = response.read()

        self.assertEqual(200, response.status_code)
        self.assertIn(b"ok", body)
        completed = self.find_event("request.completed")
        self.assertEqual("captured", completed["response_body"]["state"])
        self.assertEqual(len(body), completed["response_body"]["bytes_observed"])

    def test_a_never_read_stream_is_not_reported_as_captured(self):
        self.start_capture()

        with httpx.stream("GET", self.origin_url + "/json") as response:
            self.assertEqual(200, response.status_code)

        completed = self.find_event("request.completed")
        self.assertEqual("omitted", completed["response_body"]["state"])
        self.assertEqual("incomplete", completed["response_body"]["reason"])

    def test_async_client_is_captured(self):
        self.start_capture()

        async def call():
            async with httpx.AsyncClient() as client:
                response = await client.post(
                    self.origin_url + "/bad", json={"token": SECRET_MARKER}
                )
                return response

        response = asyncio.run(call())

        self.assertEqual(400, response.status_code)
        start = self.find_event("request.started")
        self.assertEqual("POST", start["method"])
        self.assertIn("test_adapters.py", start["call_site"])
        completed = self.find_event("request.completed")
        self.assertEqual("REDACTED", completed["response_body"]["json"]["token"])
        self.assert_payloads_are_sanitized()

    def test_async_stream_is_observed(self):
        self.start_capture()

        async def call():
            async with httpx.AsyncClient() as client:
                async with client.stream("GET", self.origin_url + "/json") as response:
                    body = await response.aread()
                    return response.status_code, body

        status, body = asyncio.run(call())

        self.assertEqual(200, status)
        self.assertIn(b"ok", body)
        completed = self.find_event("request.completed")
        self.assertEqual("captured", completed["response_body"]["state"])

    def test_a_transport_error_is_recorded_and_reraised(self):
        self.start_capture()
        port = _closed_port()

        with self.assertRaises(httpx.ConnectError):
            httpx.get("http://127.0.0.1:%d/" % port, timeout=1)

        completed = self.find_event("request.completed")
        self.assertEqual("ConnectError", completed["transport_error_class"])

    def test_a_followed_redirect_reports_the_final_url(self):
        self.start_capture()

        response = httpx.get(self.origin_url + "/redirect", follow_redirects=True)

        self.assertEqual(200, response.status_code)
        self.assertEqual(1, len(self.completed_events()))
        start = self.find_event("request.started")
        self.assertTrue(start["url"].endswith("/redirect"))
        completed = self.find_event("request.completed")
        self.assertEqual(self.origin_url + "/json", completed["final_url"])

    def test_suppressed_requests_are_not_recorded(self):
        self.start_capture()

        with _capture.suppressed():
            httpx.get(self.origin_url + "/json")

        self.assertEqual([], self.events())
