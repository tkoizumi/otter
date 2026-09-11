#!/usr/bin/env python3
"""Mock source + destination customer API for the ``customer-sync`` example.

This is a standard-library-only stand-in for two third-party services, served by
one process on one port (default ``8899``, override with ``MOCK_PORT``):

``GET /source/customers?after=<id>&limit=<n>``
    A deterministic dataset of 25 customers (ids 1..25), ordered by id, filtered
    to ``id > after`` and limited to ``limit`` (default 5)::

        {"customers": [{"id": 6, "name": "Customer 6",
                        "email": "customer6@example.com"}, ...],
         "next_after": 10}

    ``next_after`` is the id of the last returned customer, or ``null`` when the
    dataset is exhausted.

``POST /dest/customers``
    Accepts ``{"id": <int>, "name": <str>, "email": <str>}`` and returns
    ``201 {"status": "created", "id": <id>}``. Accepted customers are kept in
    memory.

``GET /dest/customers``
    Returns ``{"count": <n>, "customers": [...]}`` where ``count`` is a
    monotonically increasing count of accepted customers.

``GET /health``
    Returns ``{"status": "ok"}``.

Environment variables
---------------------

``MOCK_PORT``
    Port to listen on (default ``8899``).
``MOCK_HOST``
    Interface to bind (default ``127.0.0.1``).
``MOCK_FAIL_AFTER``
    After the destination has accepted ``n`` customers, every further
    ``POST /dest/customers`` returns ``HTTP 500`` with
    ``{"error": "simulated destination failure"}``. This is how you demonstrate
    the integration's retry + checkpoint path: the checkpoint has already
    advanced past the accepted customers, so the next run resumes instead of
    starting over. Reset on every process start; unset or ``0`` disables it.

Usage::

    python3 examples/customer-sync/mock_api.py
    MOCK_FAIL_AFTER=3 python3 examples/customer-sync/mock_api.py

One line is logged to stderr for every request.
"""

import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit

CUSTOMER_COUNT = 25
DEFAULT_LIMIT = 5
MAX_LIMIT = 100


def _int_env(name, default):
    """Read an integer environment variable, falling back to ``default``."""
    raw = os.environ.get(name)
    if raw is None or raw.strip() == "":
        return default
    try:
        return int(raw)
    except ValueError:
        sys.stderr.write("[mock-api] ignoring invalid %s=%r\n" % (name, raw))
        return default


#: 0 (or unset) disables the simulated destination failure.
FAIL_AFTER = _int_env("MOCK_FAIL_AFTER", 0)


def customer_dataset():
    """Build the deterministic customer dataset, ordered by id."""
    return [
        {
            "id": customer_id,
            "name": "Customer %d" % customer_id,
            "email": "customer%d@example.com" % customer_id,
        }
        for customer_id in range(1, CUSTOMER_COUNT + 1)
    ]


#: Source dataset; read-only after start-up.
CUSTOMERS = customer_dataset()


class DestinationStore:
    """Thread-safe in-memory destination API."""

    def __init__(self):
        self.lock = threading.Lock()
        self.by_id = {}
        self.accepted = 0

    def accept(self, customer):
        """Store ``customer``; return ``False`` when the failure threshold is hit."""
        with self.lock:
            if FAIL_AFTER > 0 and self.accepted >= FAIL_AFTER:
                return False
            self.by_id[customer["id"]] = customer
            self.accepted += 1
            return True

    def snapshot(self):
        with self.lock:
            return self.accepted, [self.by_id[key] for key in sorted(self.by_id)]


DESTINATION = DestinationStore()


class MockApiHandler(BaseHTTPRequestHandler):
    """Routes requests to the fake source and destination APIs."""

    protocol_version = "HTTP/1.1"
    server_version = "otter-mock-api/1.0"

    # -- plumbing -------------------------------------------------------

    def log_message(self, fmt, *args):
        sys.stderr.write("[mock-api] %s %s\n" % (self.address_string(), fmt % args))
        sys.stderr.flush()

    def _read_json(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b""
        if not raw:
            return None
        try:
            return json.loads(raw.decode("utf-8"))
        except ValueError:
            return None

    def _write_json(self, status, payload):
        data = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _fail(self, status, message):
        self._write_json(status, {"error": message})

    # -- routes ---------------------------------------------------------

    def do_GET(self):
        parsed = urlsplit(self.path)
        if parsed.path == "/health":
            return self._write_json(200, {"status": "ok"})
        if parsed.path == "/source/customers":
            return self._source_customers(parsed.query)
        if parsed.path == "/dest/customers":
            return self._dest_customers()
        return self._fail(404, "not found: %s" % parsed.path)

    def do_POST(self):
        parsed = urlsplit(self.path)
        if parsed.path == "/dest/customers":
            return self._dest_create()
        return self._fail(404, "not found: %s" % parsed.path)

    def _source_customers(self, query):
        params = parse_qs(query)
        try:
            after = int(params.get("after", ["0"])[0] or 0)
            limit = int(params.get("limit", [str(DEFAULT_LIMIT)])[0] or DEFAULT_LIMIT)
        except (TypeError, ValueError):
            return self._fail(400, "after and limit must be integers")
        if limit < 1:
            limit = 1
        if limit > MAX_LIMIT:
            limit = MAX_LIMIT

        page = [customer for customer in CUSTOMERS if customer["id"] > after][:limit]
        next_after = page[-1]["id"] if page else None
        return self._write_json(200, {"customers": page, "next_after": next_after})

    def _dest_create(self):
        customer = self._read_json()
        if not isinstance(customer, dict):
            return self._fail(400, "body must be a JSON object")
        customer_id = customer.get("id")
        if not isinstance(customer_id, int) or isinstance(customer_id, bool):
            return self._fail(400, "customer id must be an integer")
        for field in ("name", "email"):
            if not isinstance(customer.get(field), str):
                return self._fail(400, "customer %s must be a string" % field)

        if not DESTINATION.accept(customer):
            return self._fail(500, "simulated destination failure")
        return self._write_json(201, {"status": "created", "id": customer_id})

    def _dest_customers(self):
        count, customers = DESTINATION.snapshot()
        return self._write_json(200, {"count": count, "customers": customers})


def main():
    host = os.environ.get("MOCK_HOST", "127.0.0.1")
    try:
        port = int(os.environ.get("MOCK_PORT", "8899"))
    except ValueError:
        sys.stderr.write("[mock-api] invalid MOCK_PORT, using 8899\n")
        port = 8899

    server = ThreadingHTTPServer((host, port), MockApiHandler)
    url = "http://%s:%d" % (host, server.server_address[1])

    print("mock source+destination API listening on %s" % url)
    print("  source:      GET  %s/source/customers?after=0&limit=5" % url)
    print("  destination: POST %s/dest/customers" % url)
    print("  destination: GET  %s/dest/customers" % url)
    print("  health:      GET  %s/health" % url)
    if FAIL_AFTER > 0:
        print("simulated failure: POST /dest/customers fails after %d accepted customers" % FAIL_AFTER)
    sys.stdout.flush()

    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\nshutting down")
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
