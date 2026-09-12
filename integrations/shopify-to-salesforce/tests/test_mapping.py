"""Tests for this integration's Shopify -> Salesforce field mapping.

The mapping is the part that changes most often -- a new field, a changed
fallback, a different picklist -- so it is pinned down here rather than only
being exercised end to end. These tests import ``mapping.py`` directly, so they
do not drag in the HTTP clients at all.

    python3 -m unittest discover -s integrations/shopify-to-salesforce/tests
"""

import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
INTEGRATION_DIR = os.path.dirname(HERE)
REPO_ROOT = os.path.dirname(os.path.dirname(INTEGRATION_DIR))

for path in (os.path.join(REPO_ROOT, "lib", "python"),
             os.path.join(REPO_ROOT, "sdk", "python"),
             INTEGRATION_DIR):
    if path not in sys.path:
        sys.path.insert(0, path)

from otter_connectors.records import build_record, validate_mapping  # noqa: E402

import mapping  # noqa: E402

EXTERNAL_ID_FIELD = "Shopify_Customer_Id__c"


def customer(**overrides):
    """A Shopify customer shaped like the GraphQL response."""
    base = {
        "id": "gid://shopify/Customer/1234",
        "email": "ann@example.com",
        "firstName": "Ann",
        "lastName": "Lee",
        "phone": "+15550000001",
        "defaultAddress": {
            "address1": "1 Main St",
            "address2": "Apt 4",
            "city": "Springfield",
            "province": "Illinois",
            "provinceCode": "IL",
            "country": "United States",
            "countryCodeV2": "US",
            "zip": "62701",
        },
    }
    base.update(overrides)
    return base


def record_for(source, **options):
    field_mapping = mapping.contact_mapping(EXTERNAL_ID_FIELD, **options)
    return build_record(field_mapping, source, limits=mapping.MAX_FIELD_LENGTH)


class ContactMappingTests(unittest.TestCase):
    def test_maps_the_core_fields(self):
        record = record_for(customer())

        self.assertEqual(record["Shopify_Customer_Id__c"], "1234")
        self.assertEqual(record["FirstName"], "Ann")
        self.assertEqual(record["LastName"], "Lee")
        self.assertEqual(record["Email"], "ann@example.com")
        self.assertEqual(record["Phone"], "+15550000001")

    def test_maps_the_mailing_address(self):
        record = record_for(customer())

        self.assertEqual(record["MailingStreet"], "1 Main St\nApt 4")
        self.assertEqual(record["MailingCity"], "Springfield")
        self.assertEqual(record["MailingPostalCode"], "62701")

    def test_takes_the_external_id_from_the_gid(self):
        record = record_for(customer(id="gid://shopify/Customer/98765"))
        self.assertEqual(record["Shopify_Customer_Id__c"], "98765")

    def test_the_external_id_field_is_configurable(self):
        field_mapping = mapping.contact_mapping("Legacy_Customer_Id__c")
        record = build_record(field_mapping, customer(), limits=mapping.MAX_FIELD_LENGTH)
        self.assertIn("Legacy_Customer_Id__c", record)
        self.assertNotIn("Shopify_Customer_Id__c", record)


class RequiredFieldTests(unittest.TestCase):
    """Contact requires LastName, and Shopify does not always supply one."""

    def test_uses_the_first_name_when_there_is_no_last_name(self):
        record = record_for(customer(lastName=None, firstName="Ann"))

        self.assertEqual(record["LastName"], "Ann")
        # Not copied into both fields, which would read as "Ann Ann".
        self.assertNotIn("FirstName", record)

    def test_falls_back_to_a_placeholder_when_both_are_missing(self):
        record = record_for(customer(lastName=None, firstName=None))

        self.assertEqual(record["LastName"], "Shopify Customer 1234")
        self.assertNotIn("FirstName", record)

    def test_leaves_both_when_both_are_present(self):
        record = record_for(customer(firstName="Ann", lastName="Lee"))
        self.assertEqual((record["FirstName"], record["LastName"]), ("Ann", "Lee"))


class PicklistTests(unittest.TestCase):
    """Orgs with State/Country picklists only accept their own values."""

    def test_sends_the_iso_code_when_the_org_accepts_it(self):
        record = record_for(customer(), valid_country={"us"}, valid_state={"il"})
        self.assertEqual(record["MailingCountry"], "US")
        self.assertEqual(record["MailingState"], "IL")

    def test_sends_the_full_name_when_that_is_what_the_org_accepts(self):
        record = record_for(customer(),
                            valid_country={"united states"}, valid_state={"illinois"})
        self.assertEqual(record["MailingCountry"], "United States")
        self.assertEqual(record["MailingState"], "Illinois")

    def test_omits_a_value_the_org_does_not_accept(self):
        # Better to drop the country than to lose the customer to a 400.
        record = record_for(customer(), valid_country={"canada"}, valid_state={"ontario"})
        self.assertNotIn("MailingCountry", record)
        self.assertNotIn("MailingState", record)

    def test_sends_values_verbatim_when_the_fields_are_not_picklists(self):
        record = record_for(customer(), valid_country=None, valid_state=None)
        self.assertEqual(record["MailingCountry"], "US")
        self.assertEqual(record["MailingState"], "IL")


class OptionalAndEmptyFieldTests(unittest.TestCase):
    def test_addresses_can_be_skipped_entirely(self):
        record = record_for(customer(), sync_address=False)
        for field in ("MailingStreet", "MailingCity", "MailingState",
                      "MailingPostalCode", "MailingCountry"):
            self.assertNotIn(field, record)

    def test_a_customer_without_an_address_omits_the_address_fields(self):
        record = record_for(customer(defaultAddress=None))
        self.assertNotIn("MailingCity", record)
        self.assertEqual(record["LastName"], "Lee", "other fields are unaffected")

    def test_empty_values_are_omitted_rather_than_sent_as_null(self):
        # A sync must not blank a field a human filled in.
        record = record_for(customer(email="", phone=None))
        self.assertNotIn("Email", record)
        self.assertNotIn("Phone", record)

    def test_values_are_trimmed(self):
        record = record_for(customer(firstName="  Ann  "))
        self.assertEqual(record["FirstName"], "Ann")

    def test_over_long_values_are_truncated(self):
        record = record_for(customer(firstName="A" * 100))
        self.assertEqual(len(record["FirstName"]), mapping.MAX_FIELD_LENGTH["FirstName"])

    def test_every_mapped_field_has_a_declared_limit(self):
        # A field without a limit is silently at the mercy of the target.
        mapped = set(mapping.contact_mapping(EXTERNAL_ID_FIELD, sync_address=True))
        mapped.discard(EXTERNAL_ID_FIELD)
        self.assertEqual(mapped - set(mapping.MAX_FIELD_LENGTH), set())


class MappingShapeTests(unittest.TestCase):
    def test_the_mapping_validates(self):
        validate_mapping(mapping.contact_mapping(EXTERNAL_ID_FIELD))

    def test_the_mapped_targets_are_exactly_what_we_expect(self):
        mapped = set(mapping.contact_mapping(EXTERNAL_ID_FIELD, sync_address=True))
        self.assertEqual(mapped, {
            "Shopify_Customer_Id__c", "LastName", "FirstName", "Email", "Phone",
            "MailingStreet", "MailingCity", "MailingState", "MailingPostalCode",
            "MailingCountry",
        })


if __name__ == "__main__":
    unittest.main()
