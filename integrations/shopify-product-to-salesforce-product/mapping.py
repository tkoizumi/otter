"""How a Shopify product variant maps onto a Salesforce Product2.

**This is the file to edit when the field mapping changes.** Add an entry to
``variant_mapping()``; add the matching field to ``queries/products.graphql`` if
Shopify is not already returning it. Target lengths come from the schema
references themselves, so there is no truncation table to keep in step.

It is deliberately pure: no environment reads, no I/O, and nothing imported from
``main`` or ``source``. That keeps it unit-testable on a dict fixture and means
it could be lifted into a shared package unchanged.

Each mapping value is a source path, a ``(path, transform)`` pair, or a callable.
Only the fields that need real logic are functions; the rest are paths. The
mechanics -- stripping, dropping empties, truncating, joining, matching picklists
-- live in ``otter_connectors``.
"""

from otter_connectors.records import resolve, text
from otter_connectors.shopify import numeric_id

from schema.salesforce import Product2
from schema.shopify import ProductVariant

__all__ = ["variant_mapping"]

# Salesforce truncates silently rather than complaining, so build_record does it
# here from each target's declared length -- see Product2.Name.length. There is
# deliberately no MAX_FIELD_LENGTH table: it would only restate the schema.
# It reads the length off the mapping *key*, so it still applies to a field whose
# value is a callable.

#: What Shopify calls the auto-created variant of a product that has only one.
PLACEHOLDER_VARIANT_TITLE = "Default Title"


def variant_name(source):
    """The product's title, qualified only when it actually has variants.

    Shopify gives the lone variant of a single-variant product the title
    "Default Title", so mapping ``Name`` straight from the variant title calls
    20 of this store's 26 records "Default Title" -- a data bug that looks like
    a working sync. ``hasOnlyDefaultVariant`` is the flag that distinguishes the
    two cases.
    """
    product_title = text(resolve(source, ProductVariant.product.title))
    variant_title = text(resolve(source, ProductVariant.title))
    if resolve(source, ProductVariant.product.hasOnlyDefaultVariant):
        variant_title = ""
    if not variant_title or variant_title == PLACEHOLDER_VARIANT_TITLE:
        return product_title
    return "%s - %s" % (product_title, variant_title)


def variant_mapping():
    """Both sides are schema references, so a wrong name fails at import.

    ``numeric_id`` turns a GID into the trailing number the customers sync has
    always stored, which is why those two are ``(path, transform)`` pairs rather
    than bare paths.

    The product-side paths resolve because ``main`` hands each variant its
    parent product under ``product``; see the comment there.
    """
    return {
        Product2.Shopify_Product_Id__c: (ProductVariant.product.id, numeric_id),
        Product2.Shopify_Variant_Id__c: (ProductVariant.id, numeric_id),
        Product2.Name: variant_name,
    }
