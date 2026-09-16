"""What this integration reads from Shopify.

The query and its paging live here so that ``main.py`` only has to ask for "the
next page". Adding a field to the mapping in ``mapping.py`` usually means adding
it here too -- keep the two in step, and ``tests/test_source.py`` will remind you.

The sync is incremental because of the ``updated_at`` filter: Shopify returns
products changed at or after the watermark, and ``sortKey: UPDATED_AT`` with
cursor paging walks them in a stable order.
"""

from otter_connectors.timeutil import to_iso

__all__ = ["PRODUCT_QUERY", "fetch_page", "updated_since"]

#: Both the ISO code and the full name are requested, because orgs with
#: State/Country picklists accept one or the other and the mapping matches
#: whichever fits.
PRODUCT_QUERY = """
query Products($first: Int!, $after: String, $query: String!, $sortKey: ProductSortKeys!) {
  products(first: $first, after: $after, query: $query, sortKey: $sortKey) {
    pageInfo {
      hasNextPage
      endCursor
    }
    nodes {
      id
      title
      createdAt
      updatedAt
      variants(first: 100){
        pageInfo {
            hasNextPage
        }
        nodes {
            id
            title
            sku
            price
        }
      }
    }
  }
}
"""


def updated_since(window_start):
    """Shopify's search filter for "changed at or after this instant"."""
    return "updated_at:>'%s'" % to_iso(window_start)


def fetch_page(shopify, window_start, page_size, cursor=None, sort_key="UPDATED_AT"):
    """One page of product changed since ``window_start``.

    Returns ``(productt, page_info)``. ``page_info["endCursor"]`` is the value
    the caller checkpoints so an interrupted run can resume mid-window.
    """
    variables = {
        "first": page_size,
        "after": cursor,
        "query": updated_since(window_start),
        "sortKey": sort_key,
    }
    return shopify.connection(PRODUCT_QUERY, variables, path="products")
