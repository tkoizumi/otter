#!/usr/bin/env python3
"""A stand-in ClickHouse HTTP endpoint for check-destinations.test.sh.

Test-only. It answers the three query shapes the checker sends, from a scenario
named in FAKE_CH_SCENARIO, and it refuses a request that arrives without the
Access service tokens when FAKE_CH_REQUIRE_ACCESS=1 -- which is how the suite
proves the checker sends them rather than only claiming to.

It binds an ephemeral port and prints "PORT=<n>" so the caller never has to guess
one, and it rejects any method other than POST so the suite cannot pass against a
checker that put the statement in the URL.
"""

import os
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse

SCENARIO = os.environ.get("FAKE_CH_SCENARIO", "healthy")
# A fixed instant the suite measures against, so freshness is deterministic.
NEWEST = os.environ.get("FAKE_CH_NEWEST", "2026-10-15 06:50:00")


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self, status, body):
        payload = body.encode("utf-8")
        self.send_response(status)
        self.send_header("content-type", "text/plain; charset=utf-8")
        self.send_header("content-length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        # The checker must POST the statement, as the jobs' own client does.
        self.reply(405, "only POST is served")

    def do_POST(self):
        parsed = urlparse(self.path)
        query = (parse_qs(parsed.query).get("query") or [""])[0]

        if os.environ.get("FAKE_CH_REQUIRE_ACCESS") == "1":
            if not self.headers.get("CF-Access-Client-Id") or not self.headers.get(
                "CF-Access-Client-Secret"
            ):
                return self.reply(403, "Cloudflare Access denied")

        if SCENARIO == "forbidden":
            return self.reply(403, "Cloudflare Access denied")

        if "system.tables" in query:
            return self.reply(200, "0" if SCENARIO == "missing-table" else "1")
        if "count()" in query:
            return self.reply(200, "0" if SCENARIO == "empty" else "42")
        if "max(" in query:
            return self.reply(200, NEWEST)
        return self.reply(400, "unexpected statement: %s" % query)


if __name__ == "__main__":
    if not hasattr(HTTPServer, "server_bind"):
        sys.exit("no HTTPServer")
    server = HTTPServer(("127.0.0.1", 0), Handler)
    print("PORT=%d" % server.server_address[1], flush=True)
    server.serve_forever()
