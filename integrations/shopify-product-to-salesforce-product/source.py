"""What this integration reads from Shopify.

The queries live in ``queries/*.graphql`` as documents rather than as strings
assembled here. That is the whole point: a ``.graphql`` file is a first-class
artifact the editor understands, so you get completion against the real Shopify
schema, inline errors for a field that does not exist, and a jump-to-definition
into ``schema/shopify/shopify.graphql`` -- none of which a Python string gets you.
``graphql.config.yml`` at the repo root wires the two together. The file is also
what ``tests/test_source.py`` validates, so a query that does not match the
pulled schema fails a test rather than a 6am run.

**Products, not variants.** ``productVariants`` is the flatter connection and was
the obvious root, but a product-level change does not bump its variants'
``updatedAt``: in this store 14 of 17 products are newer than every one of their
variants. A variant-level watermark therefore misses a renamed product forever,
and the mapping reads ``product.title``. So the root is ``products`` and the
watermark is compared against ``Product.updatedAt``.

The cost is a nested connection. ``variants`` inside a product is capped by
Shopify, so ``pageInfo`` is selected and ``all_variants`` fetches the remainder
through ``product-variants.graphql``. Silently syncing the first page of a
large product is the failure mode that arrangement exists to prevent.
"""

from pathlib import Path

from otter_connectors.shopify import ShopifyError, numeric_id
from otter_connectors.timeutil import to_iso

__all__ = [
    "QUERIES",
    "PRODUCTS_QUERY",
    "VARIANTS_QUERY",
    "MAX_VARIANTS_PER_PAGE",
    "SORT_KEY",
    "all_variants",
    "fetch_page",
    "fetch_variants",
    "updated_since",
]

#: Query documents, resolved next to this module so the integration works
#: whatever directory the runner starts it from.
QUERIES = Path(__file__).parent / "queries"

PRODUCTS_QUERY = (QUERIES / "products.graphql").read_text(encoding="utf-8")
VARIANTS_QUERY = (QUERIES / "product-variants.graphql").read_text(encoding="utf-8")

#: Shopify's ceiling for a `first:` argument. Asking for more is an error rather
#: than a clamp, so the number is stated once here and used for both queries.
MAX_VARIANTS_PER_PAGE = 250

#: ``ProductSortKeys.UPDATED_AT``. Type checking does not cover a variable's
#: *value*, so the member name is named here and asserted against the schema in
#: tests -- the old flat query shipped a sort key Shopify rejected, and only
#: running it against the live API found out.
SORT_KEY = "UPDATED_AT"


def updated_since(window_start):
    """Shopify's search filter for "changed at or after this instant".

    Note that Shopify does *not* validate the field name here: an unknown field
    matches everything rather than erroring, which would turn an incremental
    sync into a full rescan. ``tests/`` asserts the shape for that reason.
    """
    return "updated_at:>'%s'" % to_iso(window_start)


def fetch_page(shopify, window_start, page_size, cursor=None):
    """One page of products changed since ``window_start``.

    Returns ``(products, page_info)``. ``page_info["endCursor"]`` is the value
    the caller checkpoints so an interrupted run can resume mid-window.
    """
    variables = {
        "first": page_size,
        "after": cursor,
        "query": updated_since(window_start),
        "sortKey": SORT_KEY,
    }
    return shopify.connection(PRODUCTS_QUERY, variables, path="products")


def fetch_variants(shopify, product_id, cursor=None, page_size=MAX_VARIANTS_PER_PAGE):
    """One page of a single product's variants, continuing the nested connection.

    ``product_id`` is a GID; Shopify's filter wants the numeric tail.
    """
    variables = {
        "first": page_size,
        "after": cursor,
        "filter": "product_id:%s" % numeric_id(product_id),
    }
    return shopify.connection(VARIANTS_QUERY, variables, path="productVariants")


def all_variants(shopify, product, page_size=MAX_VARIANTS_PER_PAGE):
    """Every variant of ``product``, past the nested page when it is truncated.

    The nested page under ``products`` is Shopify's cheapest way to fetch
    variants, but it stops at ``page_size``. ``pageInfo.hasNextPage`` says
    whether it did, so the remainder is fetched explicitly instead of assumed
    away.
    """
    connection = product.get("variants") or {}
    variants = list(connection.get("nodes") or [])
    page_info = connection.get("pageInfo") or {}
    cursor = page_info.get("endCursor")

    while page_info.get("hasNextPage"):
        more, page_info = fetch_variants(
            shopify, product.get("id"), cursor=cursor, page_size=page_size
        )
        if not more:
            raise ShopifyError(
                "Shopify reported more variants for %s but returned none" % product.get("id")
            )
        variants.extend(more)
        if page_info.get("endCursor") == cursor:
            raise ShopifyError(
                "Shopify variant paging did not advance for %s" % product.get("id")
            )
        cursor = page_info.get("endCursor")

    return variants
