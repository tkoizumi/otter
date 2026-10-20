#!/usr/bin/env python3
"""Check that each sync job's ClickHouse destination is actually being filled.

A successful Python exit says nothing about whether the rows landed. This asks
ClickHouse directly, over the same endpoint the job uses, and answers three
questions per destination:

  1. does the table exist?
  2. does it hold rows for this job's dataset id?
  3. is its newest `synced_at` recent?

Why those three, from `docs/p0-13-castor-import-migration-record.md` section 7,
which lists the places the migration is held together by agreement rather than by
a check:

  * **Seam 2, destination tables are created by a person.** The worker created
    them; jobs deliberately do not. A job pointed at a table that does not exist
    fails loudly -- but only once its first tick comes round, and "loudly" during
    a 30-day soak is 30 days later. Asking first costs one query.
  * **Seam 1, the dataset-id map is hardcoded on both sides.** A valid id naming
    the wrong dataset files every row under it *silently*. Counting rows for the
    expected dataset is the check that notices.

The third question is the one the gate actually asks for. `docs/product-roadmap.md`
requires "independently checked destination outcomes", because a run can succeed
while writing nothing, writing to the wrong place, or writing stale data. Freshness
is the cheapest honest answer.

Independence: this shares no code with the runtime and does not read its database.
It talks to ClickHouse and nothing else.

Settings come from the same environment names the jobs use:

  CLICKHOUSE_URL                   required, scheme/host/port only
  CLICKHOUSE_DATABASE              default "default"
  CLICKHOUSE_USER                  optional; sent as a header, never in the URL
  CLICKHOUSE_PASSWORD              optional
  CLICKHOUSE_ACCESS_CLIENT_ID      optional Cloudflare Access service token
  CLICKHOUSE_ACCESS_CLIENT_SECRET  optional

Exit status is 0 when every destination passes, 1 when any fails, and 2 when the
check could not be run at all (missing settings, unreadable config). The
distinction matters: "the destination is empty" and "we could not look" are
different findings and must not print the same.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

DEFAULT_CONFIG = os.path.join(os.path.dirname(os.path.abspath(__file__)), "destinations.json")
DEFAULT_FRESHNESS_COLUMN = "synced_at"


class ConfigError(Exception):
    """The check could not be run, as distinct from a destination failing."""


class ClickHouseError(ConfigError):
    """ClickHouse answered with an error."""


def parse_time(value):
    """Parse the timestamps ClickHouse prints, or None when it printed nothing."""
    text = (value or "").strip()
    if not text or text == "\\N":
        return None
    # ClickHouse prints "2026-10-15 06:50:12" by default and ISO-8601 when asked.
    for fmt in ("%Y-%m-%d %H:%M:%S", "%Y-%m-%dT%H:%M:%SZ", "%Y-%m-%dT%H:%M:%S"):
        try:
            return dt.datetime.strptime(text, fmt).replace(tzinfo=dt.timezone.utc)
        except ValueError:
            continue
    try:
        parsed = dt.datetime.fromisoformat(text.replace("Z", "+00:00"))
    except ValueError:
        return None
    return parsed if parsed.tzinfo else parsed.replace(tzinfo=dt.timezone.utc)


class ClickHouse:
    """The HTTP interface, authenticated the way the jobs authenticate.

    Credentials are headers rather than URL parts so they cannot leak through a
    log line that quotes the endpoint -- the same reason the job's own client
    does it this way.
    """

    def __init__(self, url, database, username, password, access_id, access_secret, timeout=30):
        if not url:
            raise ConfigError("CLICKHOUSE_URL is not set")
        self.url = url.rstrip("/")
        self.database = database or "default"
        self.username = username
        self.password = password
        self.access_id = access_id
        self.access_secret = access_secret
        self.timeout = timeout
        self.statements = []

    def _headers(self):
        headers = {"content-type": "text/plain; charset=utf-8"}
        if self.username:
            headers["X-ClickHouse-User"] = self.username
        if self.password:
            headers["X-ClickHouse-Key"] = self.password
        if self.access_id:
            headers["CF-Access-Client-Id"] = self.access_id
        if self.access_secret:
            headers["CF-Access-Client-Secret"] = self.access_secret
        return headers

    def query(self, sql):
        self.statements.append(sql)
        params = urllib.parse.urlencode({"database": self.database, "query": sql})
        request = urllib.request.Request(
            "%s/?%s" % (self.url, params), data=b"", headers=self._headers(), method="POST"
        )
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                return response.read().decode("utf-8", "replace")
        except urllib.error.HTTPError as error:
            body = error.read().decode("utf-8", "replace").strip()
            if error.code == 403:
                raise ClickHouseError(
                    "ClickHouse refused with 403: the endpoint is behind Cloudflare Access and "
                    "the service tokens are missing or wrong, which reads like a database "
                    "problem rather than a credential one (%s)" % (body or "no body")
                ) from error
            raise ClickHouseError("ClickHouse refused with %d: %s" % (error.code, body or "no body")) from error
        except urllib.error.URLError as error:
            raise ClickHouseError("cannot reach %s: %s" % (self.url, error.reason)) from error


def quote(value):
    """A ClickHouse string literal. The values here come from config, not a user."""
    return "'" + str(value).replace("\\", "\\\\").replace("'", "\\'") + "'"


def identifier(value):
    """A ClickHouse identifier, in backticks.

    Not the same quoting as a string literal, and the difference is a syntax error
    rather than a wrong answer: `FROM 'castor'.'shopify_products'` is rejected
    outright, because single quotes make those string literals. This was found by
    running the check against a live ClickHouse -- the stand-in in the test suite
    answers whatever it is asked, so it cannot catch a malformed statement.
    """
    return "`" + str(value).replace("`", "``") + "`"


def check_destination(client, expectation, now):
    """Return (ok, detail, facts) for one destination."""
    database = expectation["database"]
    table = expectation["table"]
    dataset_id = expectation.get("dataset_id")
    column = expectation.get("freshness_column", DEFAULT_FRESHNESS_COLUMN)
    min_rows = int(expectation.get("min_rows", 1))
    max_age = expectation.get("max_age_seconds")

    qualified = "%s.%s" % (identifier(database), identifier(table))

    exists = client.query(
        "SELECT count() FROM system.tables WHERE database = %s AND name = %s"
        % (quote(database), quote(table))
    ).strip()
    if exists != "1":
        return (
            False,
            "destination table %s.%s does not exist" % (database, table),
            {"exists": False},
        )

    where = ""
    if dataset_id:
        where = " WHERE dataset_id = %s" % quote(dataset_id)

    rows = client.query("SELECT count() FROM %s%s" % (qualified, where)).strip()
    try:
        row_count = int(rows)
    except ValueError:
        return (False, "could not read a row count from %r" % rows, {"exists": True})

    newest = parse_time(client.query("SELECT max(%s) FROM %s%s" % (identifier(column), qualified, where)))
    age = None if newest is None else (now - newest).total_seconds()

    facts = {
        "exists": True,
        "rows": row_count,
        "newest": None if newest is None else newest.isoformat().replace("+00:00", "Z"),
        "age_seconds": None if age is None else int(age),
    }

    if row_count < min_rows:
        scope = " for dataset %s" % dataset_id if dataset_id else ""
        return (False, "no rows%s (%d, wanted at least %d)" % (scope, row_count, min_rows), facts)

    if max_age is not None and age is not None and age > max_age:
        return (
            False,
            "newest %s is %ds old, past the %ds bound" % (column, int(age), int(max_age)),
            facts,
        )

    if max_age is not None and age is None:
        return (False, "column %s holds no usable timestamp" % column, facts)

    detail = "%d row(s)" % row_count
    if age is not None:
        detail += ", newest %s ago" % humanize(age)
    return (True, detail, facts)


def humanize(seconds):
    seconds = int(seconds)
    if seconds < 60:
        return "%ds" % seconds
    if seconds < 3600:
        return "%dm%ds" % (seconds // 60, seconds % 60)
    return "%dh%dm" % (seconds // 3600, (seconds % 3600) // 60)


def load_expectations(path):
    try:
        with open(path, "r", encoding="utf-8") as handle:
            raw = json.load(handle)
    except FileNotFoundError:
        raise ConfigError("no config at %s" % path) from None
    except json.JSONDecodeError as error:
        raise ConfigError("config at %s is not valid JSON: %s" % (path, error)) from None

    entries = raw.get("destinations") if isinstance(raw, dict) else raw
    if not isinstance(entries, list) or not entries:
        raise ConfigError("config at %s lists no destinations" % path)

    for entry in entries:
        # `database` is deliberately not required: it defaults to the endpoint's
        # configured database, and an entry may override it. Requiring it here
        # would make the config repeat a setting that already has an owner.
        if not entry.get("table"):
            raise ConfigError("a destination is missing table")
    return entries


def main(argv=None):
    parser = argparse.ArgumentParser(description="Check ClickHouse destinations are being filled.")
    parser.add_argument("--config", default=DEFAULT_CONFIG)
    parser.add_argument("--json", action="store_true", help="emit a machine-readable verdict")
    parser.add_argument("--now", help="RFC3339 instant to measure freshness against (for tests)")
    parser.add_argument("--timeout", type=float, default=30.0)
    args = parser.parse_args(argv)

    if args.now:
        now = parse_time(args.now)
        if now is None:
            print("check-destinations: --now is not a timestamp: %r" % args.now, file=sys.stderr)
            return 2
    else:
        now = dt.datetime.now(dt.timezone.utc)

    try:
        expectations = load_expectations(args.config)
        client = ClickHouse(
            url=os.environ.get("CLICKHOUSE_URL"),
            database=os.environ.get("CLICKHOUSE_DATABASE", "default"),
            username=os.environ.get("CLICKHOUSE_USER"),
            password=os.environ.get("CLICKHOUSE_PASSWORD"),
            access_id=os.environ.get("CLICKHOUSE_ACCESS_CLIENT_ID"),
            access_secret=os.environ.get("CLICKHOUSE_ACCESS_CLIENT_SECRET"),
            timeout=args.timeout,
        )
    except ConfigError as error:
        print("check-destinations: %s" % error, file=sys.stderr)
        return 2

    results = []
    for expectation in expectations:
        entry = dict(expectation)
        entry.setdefault("database", client.database)
        try:
            ok, detail, facts = check_destination(client, entry, now)
        except ConfigError as error:
            # "We could not look" is not "the destination is empty", and it must
            # not print as though it were.
            print("check-destinations: %s" % error, file=sys.stderr)
            return 2
        results.append(
            {
                "job": entry.get("job"),
                "table": entry["table"],
                "database": entry["database"],
                "ok": ok,
                "detail": detail,
                **facts,
            }
        )

    failed = [result for result in results if not result["ok"]]

    if args.json:
        print(
            json.dumps(
                {
                    "checked": len(results),
                    "passed": len(results) - len(failed),
                    "failed": len(failed),
                    "checked_at": now.isoformat().replace("+00:00", "Z"),
                    "results": results,
                },
                indent=2,
                sort_keys=True,
            )
        )
    else:
        width = max(len(result["table"]) for result in results)
        for result in results:
            print(
                "%-*s  %s  %s"
                % (width, result["table"], "ok  " if result["ok"] else "FAIL", result["detail"])
            )
        print(
            "check-destinations: %d checked, %d passed, %d failed"
            % (len(results), len(results) - len(failed), len(failed))
        )

    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
