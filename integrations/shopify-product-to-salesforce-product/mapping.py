"""How a Shopify product variant maps onto a Salesforce Product2.

**This is the file to edit when the field mapping changes.** Add an entry to
``variant_mapping()``; add the matching field to the query in ``source.py`` if
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

from otter_connectors.shopify import numeric_id

from schema.salesforce import Product2

__all__ = ["variant_mapping"]

# Salesforce truncates silently rather than complaining, so build_record does it
# here from each target's declared length -- see Product2.Name.length. There is
# deliberately no MAX_FIELD_LENGTH table: it would only restate the schema.


def get_product_id(variant):
    return numeric_id(variant.get("product_id"))


def get_variant_id(variant):
    return numeric_id(variant.get("id"))


def get_variant_title(variant):
    return variant.get("title")


def variant_mapping():
    mapping = {
        Product2.Shopify_Product_Id__c: get_product_id,
        Product2.Shopify_Variant_Id__c: get_variant_id,
        Product2.Name: get_variant_title,
    }
    return mapping
