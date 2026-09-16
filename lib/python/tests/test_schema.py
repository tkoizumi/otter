"""Tests for otter_schema: the Field type and the Salesforce schema generator."""

import os
import sys
import textwrap
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))

from otter_connectors.records import build_record  # noqa: E402
from otter_schema import Field, limits  # noqa: E402
from otter_schema.generate import attribute_name, render_init, render_module  # noqa: E402
from otter_schema.pull import (  # noqa: E402
    find_repo_root, object_names, read_env_file, read_manifest_env,
)

DESCRIBE = {
    "name": "Product2",
    "fields": [
        {"name": "Name", "type": "string", "length": 80,
         "createable": True, "nillable": False, "unique": False},
        {"name": "ProductCode", "type": "string", "length": 255,
         "createable": True, "nillable": True},
        {"name": "Id", "type": "id", "length": 18,
         "createable": False, "nillable": False},
        {"name": "Shopify_Variant_Id__c", "type": "string", "length": 200,
         "createable": True, "nillable": True, "externalId": True, "unique": True},
        {"name": "Description", "type": "textarea", "length": 32000,
         "createable": True, "nillable": True, "defaultedOnCreate": True},
        {"name": "Family", "type": "picklist", "length": 40, "createable": True,
         "nillable": True,
         "picklistValues": [{"value": "Hardware", "active": True},
                            {"value": "Retired", "active": False}]},
    ],
}


def render(**overrides):
    kwargs = dict(instance_url="https://example.my.salesforce.com",
                  api_version="62.0", fetched_at="2026-09-16T00:00:00Z")
    kwargs.update(overrides)
    return render_module("Product2", DESCRIBE, **kwargs)


class FieldIsAString(unittest.TestCase):
    """The whole design rests on a Field being usable wherever a string is."""

    def test_it_compares_and_hashes_as_the_plain_name(self):
        field = Field("Name", object_name="Product2", type="string", length=80)

        self.assertIsInstance(field, str)
        self.assertEqual(field, "Name")
        self.assertEqual({"Name": 1}[field], 1)
        self.assertEqual(sorted([field, "Alpha"]), ["Alpha", "Name"])

    def test_it_carries_its_own_metadata(self):
        field = Field("Shopify_Variant_Id__c", object_name="Product2",
                      type="string", length=200, external_id=True, unique=True)

        self.assertEqual(field.object_name, "Product2")
        self.assertEqual(field.length, 200)
        self.assertTrue(field.external_id)
        self.assertTrue(field.unique)
        self.assertIn("Product2", repr(field))

    def test_a_zero_length_reads_as_absent(self):
        field = Field("Id", object_name="Product2", type="id", length=0)
        self.assertIsNone(field.length)

    def test_picklist_values_default_to_absent(self):
        field = Field("Family", object_name="Product2", type="picklist")
        self.assertIsNone(field.picklist)
        self.assertTrue(field.is_picklist)

    def test_it_drops_into_the_existing_mapping_engine(self):
        """No change to otter_connectors.records is needed for this to work.

        That is the point: the mapping grammar already accepts a string as a
        key, as a path and as the first half of a (path, transform) pair.
        """
        mapping = {
            Field("Name", object_name="Product2", type="string", length=80): "title",
            Field("ProductCode", object_name="Product2", type="string"): ("sku", str.upper),
        }

        record = build_record(mapping, {"title": "A Snowboard", "sku": "abc-1"})

        self.assertEqual(record["Name"], "A Snowboard")
        self.assertEqual(record["ProductCode"], "ABC-1")
        self.assertEqual(sorted(record), ["Name", "ProductCode"])

    def test_truncation_limits_come_from_the_schema(self):
        name = Field("Name", object_name="Product2", type="string", length=5)
        mapping = {name: "title"}

        record = build_record(mapping, {"title": "abcdefghij"}, limits=limits(mapping))

        self.assertEqual(record["Name"], "abcde")

    def test_limits_ignores_fields_without_a_length(self):
        named = Field("Name", object_name="Product2", type="string", length=80)
        plain = Field("Id", object_name="Product2", type="id")

        self.assertEqual(limits({named: "a", plain: "b"}), {"Name": 80})


class GeneratorOutput(unittest.TestCase):

    def test_it_emits_a_class_with_every_field(self):
        source = render()

        for name in ("Name", "ProductCode", "Id", "Shopify_Variant_Id__c", "Family"):
            self.assertIn("%s = Field(" % name, source)
        self.assertIn("class Product2:", source)
        self.assertIn("from otter_schema import Field", source)

    def test_flags_are_carried_through(self):
        source = render()
        self.assertIn("external_id=True", source)
        self.assertIn("unique=True", source)

    def test_required_means_createable_and_not_nillable_and_not_defaulted(self):
        source = render()
        # Name: createable, not nillable, no default -> required.
        self.assertIn('Name = Field("Name", object_name="Product2", type="string", '
                      'length=80, required=True)', source)
        # Description has defaultedOnCreate, so the org fills it in.
        self.assertNotIn('"Description", object_name="Product2", type="textarea", '
                         'length=32000, required=True', source)

    def test_picklists_are_opt_in(self):
        self.assertNotIn("picklist=(", render())
        self.assertIn("picklist=(", render(include_picklists=True))

    def test_picklists_skip_inactive_values(self):
        source = render(include_picklists=True)
        self.assertIn('"Hardware"', source)
        self.assertNotIn('"Retired"', source)

    def test_output_is_deterministic_and_sorted(self):
        first = render()
        self.assertEqual(first, render())
        # Id sorts before Name sorts before ProductCode.
        self.assertLess(first.index('Id = Field('), first.index('Name = Field('))
        self.assertLess(first.index('Name = Field('), first.index('ProductCode = Field('))

    def test_the_generated_module_is_valid_python(self):
        compile(render(), "product2.py", "exec")
        compile(render_init(["Product2", "Contact"]), "__init__.py", "exec")

    def test_the_generated_module_actually_imports_and_works(self):
        """End to end: executed source, then used as a mapping."""
        namespace = {}
        exec(compile(render(), "product2.py", "exec"), namespace)  # noqa: S102
        product2 = namespace["Product2"]

        self.assertEqual(product2.Name, "Name")
        self.assertEqual(product2.Name.length, 80)
        self.assertTrue(product2.Shopify_Variant_Id__c.external_id)

        record = build_record({product2.ProductCode: "sku"}, {"sku": "TS-1"})
        self.assertEqual(record, {"ProductCode": "TS-1"})

    def test_a_long_field_wraps_rather_than_running_on(self):
        source = render()
        self.assertNotIn("Shopify_Variant_Id__c = Field(\"Shopify_Variant_Id__c\", "
                         "object_name=\"Product2\", type=\"string\", length=200, "
                         "external_id=True, unique=True)", source)
        self.assertIn("Shopify_Variant_Id__c = Field(", source)

    def test_an_empty_object_still_produces_a_class(self):
        source = render_module("Empty", {"fields": []}, instance_url="u",
                               api_version="62.0", fetched_at="t")
        self.assertIn("class Empty:", source)
        self.assertIn("pass", source)
        compile(source, "empty.py", "exec")


class AttributeNames(unittest.TestCase):

    def test_ordinary_names_pass_through(self):
        self.assertEqual(attribute_name("Shopify_Variant_Id__c"), "Shopify_Variant_Id__c")

    def test_a_python_keyword_is_suffixed(self):
        self.assertEqual(attribute_name("class"), "class_")

    def test_an_unusable_name_is_sanitised(self):
        self.assertEqual(attribute_name("has space"), "has_space")
        self.assertEqual(attribute_name("2leading"), "_2leading")

    def test_a_sanitised_name_still_carries_the_real_api_name(self):
        describe = {"fields": [{"name": "class", "type": "string", "createable": True}]}
        source = render_module("Weird", describe, instance_url="u",
                               api_version="62.0", fetched_at="t")
        self.assertIn('class_ = Field("class",', source)


class ManifestEnv(unittest.TestCase):
    def write(self, body):
        path = os.path.join(self.dir, "otter.yaml")
        with open(path, "w") as handle:
            handle.write(textwrap.dedent(body))
        return path

    def setUp(self):
        import tempfile
        self.dir = tempfile.mkdtemp()

    def test_a_missing_manifest_is_not_an_error(self):
        self.assertEqual(read_manifest_env(os.path.join(self.dir, "nope.yaml")), {})

    def test_it_reads_the_env_block_only(self):
        path = self.write("""
            version: 1
            name: demo
            python:
              mode: managed
            env:
              SALESFORCE_INSTANCE_URL: https://example.my.salesforce.com
              SALESFORCE_API_VERSION: "62.0"
              SALESFORCE_OBJECT: Product2
            trigger:
              cron: "*/5 * * * *"
        """)

        self.assertEqual(read_manifest_env(path), {
            "SALESFORCE_INSTANCE_URL": "https://example.my.salesforce.com",
            "SALESFORCE_API_VERSION": "62.0",
            "SALESFORCE_OBJECT": "Product2",
        })

    def test_it_strips_a_trailing_comment_from_an_unquoted_value(self):
        path = self.write("""
            env:
              SALESFORCE_OBJECT: Product2   # the target object
        """)
        self.assertEqual(read_manifest_env(path)["SALESFORCE_OBJECT"], "Product2")

    def test_a_manifest_without_an_env_block_yields_nothing(self):
        path = self.write("""
            name: demo
            version: 1
        """)
        self.assertEqual(read_manifest_env(path), {})

    def test_a_non_scalar_line_is_rejected_rather_than_guessed(self):
        path = self.write("""
            env:
              - bad
        """)
        with self.assertRaises(ValueError):
            read_manifest_env(path)


class ObjectNames(unittest.TestCase):
    """An integration that upserts two objects needs both schemas, and the
    Makefile can only pass one string, so both spellings have to work."""

    def test_a_single_name(self):
        self.assertEqual(object_names(["Product2"]), ["Product2"])

    def test_repeated_flags(self):
        self.assertEqual(object_names(["Contact", "Product2"]), ["Contact", "Product2"])

    def test_a_comma_separated_value(self):
        self.assertEqual(object_names(["Contact,Product2"]), ["Contact", "Product2"])

    def test_a_space_separated_value(self):
        self.assertEqual(object_names(["Contact Product2"]), ["Contact", "Product2"])

    def test_mixed_and_padded(self):
        self.assertEqual(object_names(["Contact, Product2", "Order"]),
                         ["Contact", "Product2", "Order"])

    def test_duplicates_are_dropped_but_order_is_kept(self):
        self.assertEqual(object_names(["Product2", "Contact,Product2"]),
                         ["Product2", "Contact"])

    def test_nothing_given_is_empty(self):
        self.assertEqual(object_names([]), [])


class CheckoutDiscovery(unittest.TestCase):
    """The whole point of the discovery is that the long command collapses.

    If either of these breaks, `make sync-schema` starts requiring flags again.
    """

    def setUp(self):
        import tempfile
        self.root = tempfile.mkdtemp()
        os.makedirs(os.path.join(self.root, "integrations", "demo", "schema"))

    def test_it_walks_up_to_the_checkout_root(self):
        with open(os.path.join(self.root, "go.mod"), "w") as handle:
            handle.write("module x\n")

        found = find_repo_root(os.path.join(self.root, "integrations", "demo"))

        self.assertEqual(os.path.realpath(found), os.path.realpath(self.root))

    def test_a_shared_env_file_also_marks_the_root(self):
        with open(os.path.join(self.root, "otter.env"), "w") as handle:
            handle.write("SHOPIFY_CLIENT_ID=abc\n")

        found = find_repo_root(os.path.join(self.root, "integrations", "demo"))

        self.assertEqual(os.path.realpath(found), os.path.realpath(self.root))

    def test_no_marker_is_not_an_error(self):
        import tempfile
        self.assertIsNone(find_repo_root(tempfile.mkdtemp()))


class EnvFile(unittest.TestCase):

    def write(self, body):
        import tempfile
        path = os.path.join(tempfile.mkdtemp(), "otter.env")
        with open(path, "w") as handle:
            handle.write(textwrap.dedent(body))
        return path

    def test_a_missing_file_yields_nothing(self):
        self.assertEqual(read_env_file("/nonexistent/otter.env"), {})

    def test_it_reads_comments_quotes_and_export(self):
        path = self.write("""
            # shared credentials
            SHOPIFY_CLIENT_ID=abc123
            SALESFORCE_CLIENT_SECRET="quoted value"
            export SALESFORCE_CLIENT_ID=def456
        """)

        self.assertEqual(read_env_file(path), {
            "SHOPIFY_CLIENT_ID": "abc123",
            "SALESFORCE_CLIENT_SECRET": "quoted value",
            "SALESFORCE_CLIENT_ID": "def456",
        })

    def test_an_empty_value_is_allowed_here_unlike_a_manifest(self):
        path = self.write("SHOPIFY_ACCESS_TOKEN=\n")
        self.assertEqual(read_env_file(path), {"SHOPIFY_ACCESS_TOKEN": ""})

    def test_a_line_without_an_equals_is_rejected(self):
        with self.assertRaises(ValueError):
            read_env_file(self.write("this is not a setting\n"))


if __name__ == "__main__":
    unittest.main()
