"""Ask Shopify the same question the integration asks, and print the answer.

This runs the integration's *own* query (imported from ``source.py``), so what
you see is what a run would see. It talks to Shopify only: no daemon, no run
logs, no writes anywhere.

    cd integrations/shopify-to-salesforce
    set -a; . ./.env; set +a
    SHOPIFY_STORE=your-store.myshopify.com \
      PYTHONPATH=../../lib/python python3 tools/shopify_probe.py

Options, all optional:

    --limit N        customers to print (default 1, 0 prints the whole page)
    --page-size N    per page (default the integration's PAGE_SIZE, else 20)
    --days N         look back N days instead of using the watermark (default 30)
    --sort-key KEY   UPDATED_AT or CREATED_AT
    --filter EXPR    override the search filter verbatim, e.g. "created_at:>'2026-01-01'"

Non-secret values live in ``otter.yaml``, secrets in ``.env``, so SHOPIFY_STORE
and SHOPIFY_API_VERSION are read from the environment when set and otherwise
fall back to the manifest.
"""

import argparse
import json
import os
import re
import sys
from datetime import timedelta

HERE = os.path.dirname(os.path.abspath(__file__))
INTEGRATION_DIR = os.path.dirname(HERE)
REPO_ROOT = os.path.dirname(os.path.dirname(INTEGRATION_DIR))

for path in (os.path.join(REPO_ROOT, "lib", "python"), INTEGRATION_DIR):
    if path not in sys.path:
        sys.path.insert(0, path)

from otter_connectors.clients import shopify_client  # noqa: E402
from otter_connectors.config import env, require_env  # noqa: E402
from otter_connectors.timeutil import to_iso, utcnow  # noqa: E402

from source import CUSTOMERS_QUERY, updated_since  # noqa: E402


def manifest_value(name, default=None):
    """One value out of otter.yaml, so the probe matches a real run."""
    try:
        with open(os.path.join(INTEGRATION_DIR, "otter.yaml")) as fh:
            body = fh.read()
    except OSError:
        return default
    match = re.search(r"^\s*%s:\s*(.+?)\s*$" % re.escape(name), body, re.MULTILINE)
    if not match:
        return default
    return match.group(1).strip().strip('"').strip("'")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--limit", type=int, default=1, help="0 prints every node")
    parser.add_argument("--page-size", type=int, default=None)
    parser.add_argument("--days", type=int, default=30)
    parser.add_argument("--sort-key", default="UPDATED_AT")
    parser.add_argument("--filter", dest="filter_expr", default=None)
    args = parser.parse_args()

    if not os.environ.get("SHOPIFY_STORE"):
        os.environ["SHOPIFY_STORE"] = manifest_value("SHOPIFY_STORE", "")
    if not os.environ.get("SHOPIFY_API_VERSION"):
        os.environ["SHOPIFY_API_VERSION"] = manifest_value("SHOPIFY_API_VERSION", "2026-07")

    page_size = args.page_size or int(env("PAGE_SIZE", "20"))
    window_start = utcnow() - timedelta(days=args.days)
    search = args.filter_expr or updated_since(window_start)

    client = shopify_client(require_env("SHOPIFY_STORE"))

    variables = {"first": page_size, "query": search, "sortKey": args.sort_key}
    print("filter:   %s" % search)
    print("sortKey:  %s" % args.sort_key)
    print("store:    %s" % require_env("SHOPIFY_STORE"))
    print()

    data = client.graphql(CUSTOMERS_QUERY, variables)
    connection = (data or {}).get("customers") or {}
    nodes = connection.get("nodes") or []
    page_info = connection.get("pageInfo") or {}

    shown = nodes if args.limit == 0 else nodes[: args.limit]
    for index, node in enumerate(shown):
        print("--- customer %d of %d on this page ---" % (index + 1, len(nodes)))
        # indent=2 keeps a nested address readable; sort_keys=False keeps
        # Shopify's field order, which is what makes it look like the API doc.
        print(json.dumps(node, indent=2, sort_keys=False))
        print()

    if not nodes:
        print("no customers matched. widen --days, or check the filter above.")

    print("page: %d node(s), hasNextPage=%s" % (len(nodes), page_info.get("hasNextPage")))


if __name__ == "__main__":
    main()
