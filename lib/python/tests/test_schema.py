"""Tests for otter_schema: the Field type and the Salesforce schema generator."""

import os
import sys
import textwrap
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))

from otter_connectors.records import build_record  # noqa: E402
from otter_schema import Field, Node  # noqa: E402
from otter_schema import shopify  # noqa: E402
from otter_schema.generate import (  # noqa: E402
    attribute_name, render_init, render_module,
)
from otter_schema.pull import (  # noqa: E402
    find_repo_root, main, object_names, read_env_file, read_manifest_env,
    resolve_integration,
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
    kwargs = dict(system="salesforce", integration="integrations/demo",
                  source_url="https://example.my.salesforce.com",
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

    def test_truncation_limits_come_from_the_schema_with_no_table(self):
        """The point of the whole thing: no MAX_FIELD_LENGTH to keep in step."""
        name = Field("Name", object_name="Product2", type="string", length=5)
        mapping = {name: "title"}

        record = build_record(mapping, {"title": "abcdefghij"})

        self.assertEqual(record["Name"], "abcde")

    def test_a_field_without_a_length_is_left_alone(self):
        plain = Field("Id", object_name="Product2", type="id")
        record = build_record({plain: "title"}, {"title": "x" * 300})

        self.assertEqual(len(record["Id"]), 300)

    def test_an_explicit_limit_overrides_the_schema(self):
        name = Field("Name", object_name="Product2", type="string", length=255)
        record = build_record({name: "title"}, {"title": "abcdefghij"}, limits={name: 4})

        self.assertEqual(record["Name"], "abcd")

    def test_a_none_limit_opts_a_field_out(self):
        name = Field("Name", object_name="Product2", type="string", length=5)
        record = build_record({name: "title"}, {"title": "abcdefghij"}, limits={name: None})

        self.assertEqual(record["Name"], "abcdefghij")

    def test_a_mapping_spanning_two_objects_is_rejected(self):
        from otter_connectors.records import validate_mapping
        with self.assertRaises(TypeError) as caught:
            validate_mapping({
                Field("Name", object_name="Product2", type="string"): "title",
                Field("Email", object_name="Contact", type="email"): "email",
            })
        self.assertIn("Product2", str(caught.exception))
        self.assertIn("Contact", str(caught.exception))

    def test_a_hand_written_mapping_is_not_object_checked(self):
        from otter_connectors.records import validate_mapping
        validate_mapping({"Name": "title", "Email": "email"})


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
        compile(render_init({"product2": ["Product2"], "contact": ["Contact"]}),
                "__init__.py", "exec")

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
        source = render_module("Empty", {"fields": []}, system="salesforce",
                               integration="i", source_url="u",
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
        source = render_module("Weird", describe, system="salesforce",
                               integration="i", source_url="u",
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


class GeneratedHeader(unittest.TestCase):
    """A generated file has to say where it came from and how to remake it."""

    def test_it_names_the_system_it_was_pulled_from(self):
        source = render()
        self.assertIn("System:       salesforce", source)
        self.assertIn("Source:       https://example.my.salesforce.com", source)
        self.assertIn("Fetched:      2026-09-16T00:00:00Z", source)

    def test_the_regenerate_hint_can_be_pasted(self):
        source = render()
        self.assertIn("make sync-schema INTEGRATION=integrations/demo "
                      "SYSTEM=salesforce OBJECT=Product2", source)

    def test_without_an_integration_the_hint_still_names_the_object(self):
        source = render(integration="")
        self.assertIn("make sync-schema SYSTEM=salesforce OBJECT=Product2", source)


class ResolveIntegration(unittest.TestCase):
    """`--integration` means a directory, but the Makefile prefixes the
    integrations dir and people type the bare name. Getting this wrong used to
    surface as "no instance URL", which names the wrong problem."""

    def setUp(self):
        import tempfile
        self.repo = tempfile.mkdtemp()
        self.dir = os.path.join(self.repo, "integrations", "demo")
        os.makedirs(self.dir)
        with open(os.path.join(self.dir, "otter.yaml"), "w") as handle:
            handle.write("env:\n  SALESFORCE_INSTANCE_URL: https://example.test\n")

    def test_a_directory_with_a_manifest_is_used_as_given(self):
        self.assertEqual(resolve_integration(self.dir, self.repo, self.dir), self.dir)

    def test_a_bare_name_resolves_under_the_integrations_directory(self):
        self.assertEqual(resolve_integration(os.path.join(self.repo, "demo"),
                                             self.repo, "demo"), self.dir)

    def test_a_doubled_path_says_what_went_wrong(self):
        doubled = os.path.join(self.repo, "integrations", "integrations", "demo")
        with self.assertRaises(SystemExit) as caught:
            resolve_integration(doubled, self.repo, doubled)
        message = str(caught.exception)
        self.assertIn("no otter.yaml", message)
        self.assertIn("INTEGRATION=demo", message)

    def test_a_missing_integration_is_an_error_not_a_default(self):
        missing = os.path.join(self.repo, "nope")
        with self.assertRaises(SystemExit):
            resolve_integration(missing, self.repo, "nope")


class UnknownSystem(unittest.TestCase):
    """A second system is coming. Until then, asking for one must be a clear
    error rather than a confusing failure further down."""

    def test_it_is_rejected_before_anything_network_facing(self):
        import tempfile
        with self.assertRaises(SystemExit) as caught:
            main(["--system", "netsuite", "--integration", tempfile.mkdtemp()])
        self.assertIn("netsuite", str(caught.exception))
        self.assertIn("salesforce", str(caught.exception))
        self.assertIn("shopify", str(caught.exception))


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


def scalar(name):
    return {"kind": "SCALAR", "name": name}


def obj(name):
    return {"kind": "OBJECT", "name": name}


def non_null(inner):
    return {"kind": "NON_NULL", "name": None, "ofType": inner}


def as_list(inner):
    return {"kind": "LIST", "name": None, "ofType": inner}


#: A miniature introspection result: the wrappers that actually trip people up.
TYPES = {
    "ProductVariant": [
        {"name": "sku", "type": non_null(scalar("String"))},
        {"name": "product", "type": non_null(obj("Product"))},
        {"name": "metafields", "type": non_null(obj("MetafieldConnection"))},
        {"name": "selectedOptions",
         "type": non_null(as_list(non_null(obj("SelectedOption"))))},
        {"name": "inventoryPolicy", "type": non_null({"kind": "ENUM", "name": "ProductVariantInventoryPolicy"})},
    ],
    "Product": [
        {"name": "title", "type": non_null(scalar("String"))},
        {"name": "status", "type": non_null({"kind": "ENUM", "name": "ProductStatus"})},
    ],
}


class ShopifyTypeRefs(unittest.TestCase):
    """The rules that decide what a mapping path may walk into.

    Getting these wrong is silent: a list or a connection in a path resolves to
    nothing, exactly like a typo.
    """

    def field(self, type_name, field_name):
        for entry in TYPES[type_name]:
            if entry["name"] == field_name:
                return entry["type"]
        raise AssertionError("no such field %s.%s" % (type_name, field_name))

    def test_it_unwraps_non_null(self):
        self.assertEqual(shopify.unwrap(non_null(scalar("String"))),
                         ("SCALAR", "String", False, True))

    def test_it_unwraps_a_list_of_non_null(self):
        self.assertEqual(shopify.unwrap(non_null(as_list(non_null(obj("X"))))),
                         ("OBJECT", "X", True, True))

    def test_a_singular_object_is_followable(self):
        self.assertEqual(shopify.followable(self.field("ProductVariant", "product")), "Product")

    def test_a_connection_is_not_followable(self):
        self.assertIsNone(shopify.followable(self.field("ProductVariant", "metafields")))

    def test_a_list_is_not_followable(self):
        self.assertIsNone(shopify.followable(self.field("ProductVariant", "selectedOptions")))

    def test_a_scalar_is_not_followable(self):
        self.assertIsNone(shopify.followable(self.field("ProductVariant", "sku")))

    def test_an_enum_named_like_an_object_is_not_followable(self):
        """`ProductVariantInventoryPolicy` looks like a type by name. It is an
        enum, and only the kind says so."""
        self.assertIsNone(shopify.followable(self.field("ProductVariant", "inventoryPolicy")))

    def test_module_names_are_snake_case(self):
        self.assertEqual(shopify.module_name("ProductVariant"), "product_variant")
        self.assertEqual(shopify.module_name("SEO"), "seo")
        self.assertEqual(shopify.module_name("Product"), "product")


CONNECTION = shopify.Connection("productVariants", {
    "first": shopify.Arg("Int"),
    "after": shopify.Arg("String"),
    "sortKey": shopify.Arg("ProductVariantSortKeys", values=("ID", "TITLE", "SKU")),
})


class ShopifyGeneratedModule(unittest.TestCase):

    def render(self, types=None, root="ProductVariant"):
        return shopify.render_types(
            root, types if types is not None else TYPES,
            integration="integrations/demo", source_url="store.myshopify.com",
            api_version="2026-07", fetched_at="2026-09-16T00:00:00Z")

    def namespace(self):
        namespace = {"Field": Field, "Node": Node}
        exec(compile(self.render(), "generated.py", "exec"), namespace)  # noqa: S102
        return namespace

    def test_it_is_valid_python(self):
        compile(self.render(), "product_variant.py", "exec")

    def test_a_flat_field_resolves_to_its_name(self):
        self.assertEqual(str(self.namespace()["ProductVariant"].sku), "sku")

    def test_a_nested_field_carries_its_path(self):
        """The whole reason generated types are instances rather than classes."""
        namespace = self.namespace()
        self.assertEqual(str(namespace["ProductVariant"].product.title), "product.title")

    def test_a_connection_field_is_not_generated(self):
        with self.assertRaises(AttributeError):
            self.namespace()["ProductVariant"].metafields

    def test_a_list_field_is_not_generated(self):
        with self.assertRaises(AttributeError):
            self.namespace()["ProductVariant"].selectedOptions

    def test_the_error_names_the_schema_type_not_the_generated_class(self):
        with self.assertRaises(AttributeError) as caught:
            self.namespace()["ProductVariant"].featuredImage
        self.assertIn("ProductVariant", str(caught.exception))
        self.assertNotIn("_ProductVariant", str(caught.exception))

    def test_nested_references_drive_a_real_mapping(self):
        namespace = self.namespace()
        product_variant = namespace["ProductVariant"]

        mapping = {"ProductCode": product_variant.sku,
                   "Name": product_variant.product.title}
        record = build_record(mapping, {"sku": "TS-1", "product": {"title": "Tee"}})

        self.assertEqual(record, {"ProductCode": "TS-1", "Name": "Tee"})

    def test_it_exports_instances_so_chaining_works(self):
        namespace = self.namespace()
        self.assertIsInstance(namespace["ProductVariant"], Node)
        self.assertIsInstance(namespace["ProductVariant"].product, Node)

    def test_connections_are_recorded_per_type_not_just_the_root(self):
        """``ROOT`` is what you read to hand-write a ``.graphql`` document: it
        names the connection a type is reached through and the arguments it
        takes, so the root name and its paging arguments are never guessed."""
        source = shopify.render_types(
            "ProductVariant", TYPES, integration="i", source_url="s",
            api_version="2026-07", fetched_at="t",
            connections={"ProductVariant": CONNECTION,
                         "Product": shopify.Connection("products", {})})
        self.assertIn("_ProductVariant.ROOT", source)
        self.assertIn("_Product.ROOT", source)
        self.assertIn("values=('ID', 'TITLE', 'SKU',)", source)


try:
    import graphql  # noqa: F401
    HAVE_GRAPHQL = True
except ImportError:  # pragma: no cover - depends on the dev extra
    HAVE_GRAPHQL = False

#: The smallest introspection result build_client_schema accepts.
TINY_SCHEMA = {
    "queryType": {"name": "Query"},
    "types": [
        {"kind": "OBJECT", "name": "Query", "interfaces": [], "fields": [
            {"name": "products", "args": [
                {"name": "first", "type": {"kind": "SCALAR", "name": "Int"}},
                {"name": "sortKey", "type": {"kind": "ENUM", "name": "ProductSortKeys"}},
            ], "type": {"kind": "OBJECT", "name": "ProductConnection"}},
        ]},
        {"kind": "OBJECT", "name": "ProductConnection", "interfaces": [], "fields": [
            {"name": "nodes", "args": [],
             "type": {"kind": "LIST", "ofType": {"kind": "OBJECT", "name": "Product"}}},
        ]},
        {"kind": "OBJECT", "name": "Product", "interfaces": [], "fields": [
            {"name": "title", "args": [], "type": {"kind": "SCALAR", "name": "String"}},
        ]},
        {"kind": "ENUM", "name": "ProductSortKeys", "enumValues": [
            {"name": "UPDATED_AT"}, {"name": "TITLE"},
        ]},
        {"kind": "SCALAR", "name": "String"},
        {"kind": "SCALAR", "name": "Int"},
    ],
}


class SdlRendering(unittest.TestCase):

    def render(self):
        return shopify.render_sdl(
            TINY_SCHEMA, integration="integrations/demo",
            source_url="store.myshopify.com", api_version="2026-07",
            fetched_at="2026-09-16T00:00:00Z")

    def test_the_header_says_where_it_came_from(self):
        """Checked without graphql-core: the header is ours, the SDL is not."""
        header = shopify.sdl_header(
            integration="integrations/demo", source_url="store.myshopify.com",
            api_version="2026-07", fetched_at="2026-09-16T00:00:00Z")
        self.assertTrue(header.startswith("# Generated by `make sync-schema`"))
        self.assertIn("# System:       shopify", header)
        self.assertIn("# API version:  2026-07", header)
        self.assertIn("make sync-schema INTEGRATION=integrations/demo", header)

    @unittest.skipUnless(HAVE_GRAPHQL, "graphql-core is a development extra")
    def test_the_rendered_file_starts_with_that_header(self):
        self.assertTrue(self.render().startswith("# Generated by `make sync-schema`"))

    @unittest.skipUnless(HAVE_GRAPHQL, "graphql-core is a development extra")
    def test_it_renders_real_sdl(self):
        source = self.render()
        self.assertIn("type Product {", source)
        self.assertIn("enum ProductSortKeys {", source)
        self.assertIn("products(", source)

    @unittest.skipUnless(HAVE_GRAPHQL, "graphql-core is a development extra")
    def test_the_output_is_a_schema_an_editor_can_use(self):
        """The point of the file: a language server validates documents with it."""
        from graphql import build_schema, parse, validate

        schema = build_schema(self.render())
        self.assertEqual(validate(schema, parse(
            "query { products(first: 1, sortKey: UPDATED_AT) { nodes { title } } }")), [])

    @unittest.skipUnless(HAVE_GRAPHQL, "graphql-core is a development extra")
    def test_a_field_typo_is_caught_offline(self):
        from graphql import build_schema, parse, validate

        schema = build_schema(self.render())
        errors = validate(schema, parse("query { products(first: 1) { nodes { titel } } }"))
        self.assertTrue(errors)
        self.assertIn("titel", errors[0].message)

    @unittest.skipUnless(HAVE_GRAPHQL, "graphql-core is a development extra")
    def test_a_bad_enum_value_is_caught_offline(self):
        """UPDATED_AT is not a valid sort key for a variant connection, which
        cost a run to discover before there was a schema to check against."""
        from graphql import build_schema, parse, validate

        schema = build_schema(self.render())
        errors = validate(schema, parse(
            "query { products(first: 1, sortKey: NONSENSE) { nodes { title } } }"))
        self.assertTrue(errors)
        self.assertIn("NONSENSE", errors[0].message)
