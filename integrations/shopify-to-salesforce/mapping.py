"""How a Shopify customer maps onto a Salesforce Contact.

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

from otter_connectors.records import joined, text
from otter_connectors.salesforce import pick_allowed
from otter_connectors.shopify import numeric_id

__all__ = ["MAX_FIELD_LENGTH", "contact_mapping"]

#: Salesforce truncates silently rather than complaining, so do it here.
#: Every mapped target should appear, so a long value cannot cost the record.
MAX_FIELD_LENGTH = {
    "FirstName": 40,
    "LastName": 80,
    "Email": 80,
    "Phone": 40,
    "MailingStreet": 255,
    "MailingCity": 40,
    "MailingState": 80,
    "MailingPostalCode": 20,
    "MailingCountry": 80,
}


def contact_external_id(customer):
    """The upsert key: Shopify's numeric customer id."""
    return numeric_id(customer.get("id"))


def contact_names(customer):
    """``(first, last)`` for a Contact, satisfying its required LastName.

    When Shopify only has a first name it becomes the last name, rather than
    being copied into both fields.
    """
    first = text(customer.get("firstName"))
    last = text(customer.get("lastName"))
    if last:
        return first, last
    if first:
        return "", first
    return "", "Shopify Customer %s" % numeric_id(customer.get("id"))


def contact_first_name(customer):
    return contact_names(customer)[0]


def contact_last_name(customer):
    return contact_names(customer)[1]


def address_picklist(field, alternative, allowed):
    """Take a Shopify address value the org's picklist will actually accept.

    Orgs with State/Country picklists reject values that are not on their list,
    and Shopify supplies both an ISO code and a full name, so try both. When
    neither is accepted the field is omitted rather than sent invalid -- losing
    the country is better than losing the customer.
    """
    def extract(customer):
        address = customer.get("defaultAddress") or {}
        return pick_allowed(address.get(field), address.get(alternative), allowed)

    return extract


def contact_mapping(external_id_field, sync_address=True,
                    valid_country=None, valid_state=None):
    """How a Shopify customer maps onto a Salesforce Contact, as data.

    ``valid_country`` and ``valid_state`` are the org's own picklist values, as
    returned by ``SalesforceClient.picklist_values``; pass ``None`` when the
    org has no State/Country picklists.
    """
    mapping = {
        external_id_field: contact_external_id,
        "LastName": contact_last_name,
        "FirstName": contact_first_name,
        "Email": "email",
        "Phone": "phone",
    }
    if sync_address:
        mapping.update({
            "MailingStreet": joined("defaultAddress.address1", "defaultAddress.address2"),
            "MailingCity": "defaultAddress.city",
            "MailingState": address_picklist("provinceCode", "province", valid_state),
            "MailingPostalCode": "defaultAddress.zip",
            "MailingCountry": address_picklist("countryCodeV2", "country", valid_country),
        })
    return mapping
