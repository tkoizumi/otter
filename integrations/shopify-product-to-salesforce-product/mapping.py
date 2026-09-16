"""How a Shopify product maps onto a Salesforce Contact.

**This is the file to edit when the field mapping changes.** Add an entry to
``contact_mapping()``, and a length to ``MAX_FIELD_LENGTH`` if the target field
has one; add the matching field to the query in ``source.py`` if Shopify is not
already returning it.

It is deliberately pure: no environment reads, no I/O, and nothing imported from
``main`` or ``source``. That keeps it unit-testable on a dict fixture and means
it could be lifted into a shared package unchanged.

Each mapping value is a source path, a ``(path, transform)`` pair, or a callable.
Only the fields that need real logic are functions; the rest are paths. The
mechanics -- stripping, dropping empties, truncating, joining, matching picklists
-- live in ``otter_connectors``.
"""

from otter_connectors.shopify import numeric_id

__all__ = ["MAX_FIELD_LENGTH", "variant_mapping"]

#: Salesforce truncates silently rather than complaining, so do it here.
#: Every mapped target should appear, so a long value cannot cost the record.
MAX_FIELD_LENGTH = {
    # 200 is the field's declared length in the org; Salesforce truncates
    # silently, so declaring more than it holds would defeat this table.
    "Shopify_Product_Id__c": 200,
    "Shopify_Variant_Id__c": 200,
    "Name": 200,
}


def get_product_id(variant):
    return numeric_id(variant.get("product_id"))


def get_variant_id(variant):
    return numeric_id(variant.get("id"))


def get_variant_title(variant):
    return variant.get("title")


def variant_mapping():
    mapping = {
        "Shopify_Product_Id__c": get_product_id,
        "Shopify_Variant_Id__c": get_variant_id,
        "Name": get_variant_title,
    }
    return mapping
