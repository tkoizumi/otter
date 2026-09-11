#!/usr/bin/env python3
"""Sync customers from a mock source API into a mock destination API.

This example is the "resumable integration" pattern: read a checkpoint from
Otter state, page through the source, push each customer to the destination, and
persist the checkpoint **after every single customer** (not after every page).
If the process dies half way through a page, the next run resumes from the last
customer that was actually delivered instead of starting over.

The mock API is served by ``mock_api.py`` on ``http://127.0.0.1:8899``.

How to run it
-------------

1. Start the mock source + destination API in a first terminal::

       python3 examples/customer-sync/mock_api.py

2. Run the integration once in a second terminal::

       otter run customer-sync

   The first run syncs all 25 customers and prints::

       synced 25 customers; checkpoint=25

   Running it again syncs nothing (the checkpoint is already at the end) and
   prints ``synced 0 customers; checkpoint=25``.

3. Demonstrate resume-on-crash. ``CRASH_AFTER=<n>`` makes the integration raise
   on purpose after ``n`` customers have been delivered and checkpointed in the
   current run::

       CRASH_AFTER=3 otter run customer-sync     # exits non-zero, checkpoint=3
       otter run customer-sync                   # resumes at customer 4

   Otter's retry policy (``attempts: 3``) reruns the integration; each attempt
   continues from the checkpoint left behind by the previous one.

4. Demonstrate retry-on-failure. Restart the mock API with
   ``MOCK_FAIL_AFTER=<n>`` so the destination starts returning HTTP 500 after
   ``n`` accepted customers::

       MOCK_FAIL_AFTER=5 python3 examples/customer-sync/mock_api.py
       otter run customer-sync

   The integration exits non-zero with a clear error, the checkpoint stays at
   the last delivered customer, and Otter retries with backoff.

Environment variables
---------------------

``SOURCE_API_URL`` / ``DEST_API_URL``
    Base URLs of the two services (set by ``otter.yaml``; both default to
    ``http://127.0.0.1:8899``).
``CRASH_AFTER``
    Deliberately crash after this many customers processed in the current run.
    Unset or ``0`` disables it.
"""

import http.client
import json
import os
import urllib.error
import urllib.request

from otter import run

PAGE_SIZE = 5
DEFAULT_API_URL = "http://127.0.0.1:8899"


def _int_env(name, default=0):
    """Read an integer environment variable, falling back to ``default``."""
    raw = os.environ.get(name)
    if raw is None or raw.strip() == "":
        return default
    try:
        return int(raw)
    except ValueError:
        return default


def _request_json(method, url, payload=None, timeout=15.0):
    """Perform one JSON HTTP request, returning ``(status, decoded_body)``.

    Connection level problems raise ``urllib.error.URLError`` / ``OSError`` and
    are translated by the callers into clear ``RuntimeError`` messages.
    """
    body = None if payload is None else json.dumps(payload).encode("utf-8")
    request = urllib.request.Request(url, data=body, method=method)
    request.add_header("Accept", "application/json")
    if body is not None:
        request.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            raw = response.read()
            return int(response.status), (json.loads(raw.decode("utf-8")) if raw else None)
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        try:
            decoded = json.loads(raw.decode("utf-8")) if raw else None
        except (ValueError, UnicodeDecodeError):
            decoded = None
        return int(exc.code), decoded


def _fetch_page(source_url, after):
    """Fetch one page of customers; returns ``(customers, next_after)``."""
    url = "%s/source/customers?after=%d&limit=%d" % (
        source_url.rstrip("/"),
        after,
        PAGE_SIZE,
    )
    try:
        status, body = _request_json("GET", url)
    except (urllib.error.URLError, http.client.HTTPException, OSError) as exc:
        raise RuntimeError(
            "source API unreachable at %s (%s); start it first with: "
            "python3 examples/customer-sync/mock_api.py" % (url, exc)
        )
    if status != 200 or not isinstance(body, dict):
        raise RuntimeError("source API returned HTTP %s for %s: %r" % (status, url, body))
    customers = body.get("customers") or []
    return customers, body.get("next_after")


def _push_customer(dest_url, customer):
    """POST one customer to the destination, raising on any non-2xx response."""
    url = "%s/dest/customers" % dest_url.rstrip("/")
    try:
        status, body = _request_json("POST", url, customer)
    except (urllib.error.URLError, http.client.HTTPException, OSError) as exc:
        raise RuntimeError("destination API unreachable at %s: %s" % (url, exc))
    if not 200 <= status < 300:
        raise RuntimeError(
            "destination API rejected customer %s with HTTP %s: %r"
            % (customer.get("id"), status, body)
        )


@run
def main(ctx):
    """Page through the source, deliver each customer, checkpoint after each one."""
    source_url = os.environ.get("SOURCE_API_URL") or DEFAULT_API_URL
    dest_url = os.environ.get("DEST_API_URL") or DEFAULT_API_URL
    crash_after = _int_env("CRASH_AFTER", 0)

    checkpoint = ctx.state.get("last_processed_customer_id", 0) or 0
    ctx.log.info(
        "customer sync starting",
        checkpoint=checkpoint,
        source=source_url,
        destination=dest_url,
        crash_after=crash_after,
    )

    synced = 0
    while True:
        page_start = checkpoint
        customers, next_after = _fetch_page(source_url, checkpoint)
        if not customers:
            break

        for customer in customers:
            if not isinstance(customer, dict) or not isinstance(customer.get("id"), int):
                raise RuntimeError("source API returned a malformed customer: %r" % (customer,))

            _push_customer(dest_url, customer)

            # Advance the checkpoint immediately: a crash later in this page must
            # resume after this customer, not from the start of the page.
            checkpoint = customer["id"]
            ctx.state.set("last_processed_customer_id", checkpoint)
            synced += 1
            ctx.log.info(
                "customer synced",
                customer_id=checkpoint,
                name=customer.get("name"),
                checkpoint=checkpoint,
            )

            if crash_after and synced >= crash_after:
                raise RuntimeError("simulated crash after %d customers" % synced)

        if next_after is None:
            break
        if next_after <= page_start:
            raise RuntimeError(
                "source API did not advance: next_after=%r, checkpoint=%r"
                % (next_after, page_start)
            )

    print("synced %d customers; checkpoint=%d" % (synced, checkpoint))
    ctx.log.info("customer sync finished", synced=synced, checkpoint=checkpoint)
