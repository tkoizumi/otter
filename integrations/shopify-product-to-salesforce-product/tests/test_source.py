"""Tests for the Shopify side of this integration: the documents, paging, mapping.

The query is a ``.graphql`` file, so most of the drift this suite used to guard
against is gone: the editor validates it against the pulled schema as you type,
and the tests below validate the same document with ``graphql-core``, so a field
that does not exist is an error rather than an empty result. What tests still
earn their place here:

* the documents validate against ``schema/shopify/shopify.graphql``, so a query
  edited months from now fails here rather than at 6am -- and the check itself is
  proved to have teeth by a deliberate typo;
* every source path the mapping reads is a path the document actually selects,
  because an unselected path resolves to ``""`` silently and the run still
  reports success;
* the nested ``variants`` connection is paged past rather than truncated;
* the name rule treats Shopify's "Default Title" placeholder as what it is.

``graphql-core`` is an optional dev dependency (``pip install -e
'lib/python[dev]'``); the tests that need it skip without it.
"""

import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
INTEGRATION_DIR = os.path.dirname(HERE)
REPO_ROOT = os.path.dirname(os.path.dirname(INTEGRATION_DIR))

for path in (os.path.join(REPO_ROOT, "lib", "python"), INTEGRATION_DIR):
    if path not in sys.path:
        sys.path.insert(0, path)

import source  # noqa: E402
from mapping import variant_mapping  # noqa: E402

from otter_connectors.records import build_record, validate_mapping  # noqa: E402
from otter_connectors.shopify import ShopifyError  # noqa: E402
from otter_connectors.timeutil import parse_iso  # noqa: E402

try:
    from graphql import build_schema, validate
    from graphql.language import FieldNode, parse
except ImportError:  # pragma: no cover - exercised by the skip below
    build_schema = validate = parse = FieldNode = None

SDL_PATH = os.path.join(INTEGRATION_DIR, "schema", "shopify", "shopify.graphql")

_SCHEMA = []


def schema():
    """The pulled Shopify schema, built once for the whole module."""
    if not _SCHEMA:
        with open(SDL_PATH, encoding="utf-8") as handle:
            _SCHEMA.append(build_schema(handle.read()))
    return _SCHEMA[0]


def root(node):
    """The first operation of a parsed document, or the node itself."""
    definitions = getattr(node, "definitions", None)
    return definitions[0] if definitions else node


def field(node, name):
    """The named field selected by ``node``, or ``None``."""
    selection_set = getattr(root(node), "selection_set", None)
    if selection_set is None:
        return None
    for selection in selection_set.selections:
        if isinstance(selection, FieldNode) and selection.name.value == name:
            return selection
    return None


def selected(node):
    """The names selected directly by ``node``."""
    return {
        selection.name.value
        for selection in root(node).selection_set.selections
        if isinstance(selection, FieldNode)
    }


def paths_under_products(document):
    """Every source path the document offers a variant, in the mapping's terms.

    The document roots at ``products`` but the mapping is written against a
    variant, and ``main`` hands each variant its parent under ``product``. So a
    variant's paths are its own selected fields, plus ``product.<field>`` for
    each field selected on the product it sits beneath.
    """
    product_nodes = field(field(document, "products"), "nodes")
    variant_nodes = field(field(product_nodes, "variants"), "nodes")
    return selected(variant_nodes) | {
        "product." + name for name in selected(product_nodes)
    }


def mapping_source_paths(mapping):
    """The source paths a mapping reads, as strings.

    Callables are opaque -- nothing can see what a function reads -- so they are
    skipped, and ``test_the_name_rule_reads_fields_the_document_selects`` names
    the one computed field's reads by hand instead.
    """
    paths = set()
    for spec in mapping.values():
        if isinstance(spec, str):
            paths.add(str(spec))
        elif isinstance(spec, tuple) and spec:
            paths.add(str(spec[0]))
    return paths


class FakeShopify:
    """A Shopify client returning staged pages and recording what was asked."""

    def __init__(self, *pages):
        self.pages = list(pages)
        self.calls = []

    def connection(self, query, variables=None, path="customers"):
        self.calls.append({"query": query, "variables": variables, "path": path})
        if not self.pages:
            raise AssertionError("asked for more pages than the test staged")
        return self.pages.pop(0)


def product(variant_ids, has_next=False, end_cursor=None, **extra):
    document = {
        "id": "gid://shopify/Product/9",
        "variants": {
            "nodes": [{"id": variant_id} for variant_id in variant_ids],
            "pageInfo": {"hasNextPage": has_next, "endCursor": end_cursor},
        },
    }
    document.update(extra)
    return document


@unittest.skipIf(parse is None, "graphql-core is not installed")
class DocumentAgainstSchema(unittest.TestCase):
    """The documents are validated against the schema they were written for.

    This is the point of keeping the query as a ``.graphql`` file: the same
    artifact the editor completes and validates is the one CI validates, so a
    renamed Shopify field cannot reach production as an empty result.
    """

    def test_the_products_document_is_valid(self):
        errors = validate(schema(), parse(source.PRODUCTS_QUERY))
        self.assertEqual([str(e) for e in errors], [])

    def test_the_variants_document_is_valid(self):
        errors = validate(schema(), parse(source.VARIANTS_QUERY))
        self.assertEqual([str(e) for e in errors], [])

    def test_the_validation_would_catch_a_renamed_field(self):
        """Without this, the two tests above would pass on an empty check."""
        document = parse(source.PRODUCTS_QUERY.replace("sku", "skew"))
        errors = [str(e) for e in validate(schema(), document)]
        self.assertTrue(errors, "a bogus field was accepted; the check is toothless")
        self.assertTrue(any("skew" in error for error in errors), errors)


@unittest.skipIf(parse is None, "graphql-core is not installed")
class FetchList(unittest.TestCase):
    """Every path the mapping reads must be a path the document selects.

    This is the one silent failure the schema check cannot catch: the path is
    well-formed, the query is valid, and the value is simply absent -- so the
    field is dropped and the run reports success.
    """

    def paths(self):
        return paths_under_products(parse(source.PRODUCTS_QUERY))

    def test_the_mapping_only_reads_selected_fields(self):
        missing = mapping_source_paths(variant_mapping()) - self.paths()
        self.assertEqual(
            missing, set(),
            "the mapping reads %s, which the document never selects -- it would "
            "resolve to nothing" % sorted(missing))

    def test_the_name_rule_reads_fields_the_document_selects(self):
        """``variant_name`` is a callable, so the check above cannot see it."""
        paths = self.paths()
        for path in ("title", "product.title", "product.hasOnlyDefaultVariant"):
            self.assertIn(path, paths, "variant_name reads %s" % path)

    def test_the_document_offers_the_product_subtree_under_product(self):
        paths = self.paths()
        self.assertIn("product.id", paths)
        self.assertIn("id", paths)


@unittest.skipIf(parse is None, "graphql-core is not installed")
class DocumentShape(unittest.TestCase):
    """The shape decisions, asserted on the parsed document rather than on text."""

    def test_products_are_the_root_not_variants(self):
        """A product-level change does not bump its variants' updatedAt, and
        the mapping reads product.title -- so a variant-level watermark would
        never re-sync a renamed product."""
        self.assertIsNotNone(field(parse(source.PRODUCTS_QUERY), "products"))
        self.assertNotIn("productVariants(", source.PRODUCTS_QUERY)

    def test_the_watermark_is_compared_against_the_product(self):
        document = parse(source.PRODUCTS_QUERY)
        product_nodes = field(field(document, "products"), "nodes")
        self.assertIn("updatedAt", selected(product_nodes))

    def test_the_sort_key_is_one_the_enum_accepts(self):
        """Validation cannot catch this: a variable's *value* is not checked
        against the enum, so a bad sort key fails only against the live API --
        which is how the old flat query shipped with one Shopify rejected."""
        enum = schema().get_type("ProductSortKeys")
        self.assertIn(source.SORT_KEY, list(enum.values))

    def test_the_nested_page_size_matches_the_constant(self):
        """One number, stated once: Shopify errors on ``first`` above its cap
        rather than clamping, so the document and the constant cannot drift."""
        document = parse(source.PRODUCTS_QUERY)
        variants = field(field(field(document, "products"), "nodes"), "variants")
        first = next(
            argument for argument in variants.arguments if argument.name.value == "first"
        )
        self.assertEqual(int(first.value.value), source.MAX_VARIANTS_PER_PAGE)

    def test_page_info_is_selected_on_both_connections(self):
        document = parse(source.PRODUCTS_QUERY)
        product_nodes = field(field(document, "products"), "nodes")
        for connection in (field(document, "products"), field(product_nodes, "variants")):
            self.assertEqual(
                selected(field(connection, "pageInfo")),
                {"hasNextPage", "endCursor"},
                "the caller cannot page past a connection without endCursor",
            )


class VariantOverflow(unittest.TestCase):
    """``variants`` inside a product is capped, so it is paged past, not trusted."""

    def test_a_product_within_the_cap_is_not_queried_again(self):
        shopify = FakeShopify()
        found = source.all_variants(shopify, product(["a", "b"]))
        self.assertEqual([variant["id"] for variant in found], ["a", "b"])
        self.assertEqual(shopify.calls, [], "an un-truncated page needs no second query")

    def test_a_truncated_page_is_followed_to_the_end(self):
        shopify = FakeShopify(([{"id": "c"}], {"hasNextPage": False, "endCursor": "c"}))
        found = source.all_variants(shopify, product(["a", "b"], True, "b"))
        self.assertEqual([variant["id"] for variant in found], ["a", "b", "c"])

    def test_it_keeps_going_while_has_next_page_is_true(self):
        shopify = FakeShopify(
            ([{"id": "c"}], {"hasNextPage": True, "endCursor": "c"}),
            ([{"id": "d"}], {"hasNextPage": False, "endCursor": "d"}),
        )
        found = source.all_variants(shopify, product(["a"], True, "a"))
        self.assertEqual([variant["id"] for variant in found], ["a", "c", "d"])
        self.assertEqual(len(shopify.calls), 2)

    def test_the_follow_up_query_filters_on_the_numeric_product_id(self):
        shopify = FakeShopify(([{"id": "c"}], {"hasNextPage": False, "endCursor": "c"}))
        source.all_variants(shopify, product(["a"], True, "a"))
        call = shopify.calls[0]
        self.assertEqual(call["path"], "productVariants")
        self.assertEqual(call["variables"]["filter"], "product_id:9")
        self.assertEqual(call["variables"]["after"], "a")
        self.assertEqual(call["query"], source.VARIANTS_QUERY)

    def test_a_product_with_no_variants_is_empty(self):
        self.assertEqual(source.all_variants(FakeShopify(), {"id": "gid://x/1"}), [])

    def test_a_non_advancing_cursor_is_an_error_not_a_loop(self):
        shopify = FakeShopify(([{"id": "c"}], {"hasNextPage": True, "endCursor": "a"}))
        with self.assertRaises(ShopifyError):
            source.all_variants(shopify, product(["a"], True, "a"))

    def test_a_truncated_page_that_returns_nothing_is_an_error(self):
        """Shopify saying "more" and then sending none is a bug on one side or
        the other; syncing a short product silently is the worst outcome."""
        shopify = FakeShopify(([], {"hasNextPage": False, "endCursor": "a"}))
        with self.assertRaises(ShopifyError):
            source.all_variants(shopify, product(["a"], True, "a"))


class Filter(unittest.TestCase):

    def test_the_filter_has_the_shape_shopify_expects(self):
        """Shopify does not validate a filter field name -- an unknown one
        matches everything, turning an incremental sync into a full rescan."""
        rendered = source.updated_since(parse_iso("2026-01-01T00:00:00Z"))
        self.assertTrue(rendered.startswith("updated_at:>'"), rendered)
        self.assertTrue(rendered.endswith("'"), rendered)
        self.assertIn("2026-01-01T00:00:00", rendered)

    def test_the_page_request_carries_the_filter_and_the_sort_key(self):
        shopify = FakeShopify(([], {}))
        source.fetch_page(shopify, parse_iso("2026-01-01T00:00:00Z"), 25, cursor="c")
        variables = shopify.calls[0]["variables"]
        self.assertEqual(variables["first"], 25)
        self.assertEqual(variables["after"], "c")
        self.assertEqual(variables["sortKey"], "UPDATED_AT")
        self.assertEqual(variables["query"], source.updated_since(
            parse_iso("2026-01-01T00:00:00Z")))


def variant_document(title="Blue / Medium", **product_fields):
    """A variant as ``main`` hands it to the record builder."""
    product_fields.setdefault("id", "gid://shopify/Product/9")
    product_fields.setdefault("title", "The Hidden Snowboard")
    product_fields.setdefault("hasOnlyDefaultVariant", False)
    return {
        "id": "gid://shopify/ProductVariant/555",
        "sku": "TS-BLU-M",
        "title": title,
        "price": "25.00",
        "updatedAt": "2026-09-15T04:00:00Z",
        "product": product_fields,
    }


class Records(unittest.TestCase):

    def test_the_mapping_is_well_formed(self):
        validate_mapping(variant_mapping())

    def test_it_builds_the_record_the_org_expects(self):
        record = build_record(variant_mapping(), variant_document())
        self.assertEqual(record["Shopify_Variant_Id__c"], "555")
        self.assertEqual(record["Shopify_Product_Id__c"], "9")
        self.assertEqual(record["Name"], "The Hidden Snowboard - Blue / Medium")

    def test_the_ids_are_the_trailing_number_not_the_gid(self):
        record = build_record(variant_mapping(), variant_document())
        for key in ("Shopify_Variant_Id__c", "Shopify_Product_Id__c"):
            self.assertNotIn("gid://", record[key])

    def test_a_single_variant_product_is_named_after_the_product(self):
        """Shopify titles the only variant of a one-variant product "Default
        Title"; naming 20 of 26 records that looks like a working sync."""
        record = build_record(
            variant_mapping(),
            variant_document("Default Title", hasOnlyDefaultVariant=True),
        )
        self.assertEqual(record["Name"], "The Hidden Snowboard")

    def test_the_placeholder_title_is_dropped_even_unflagged(self):
        record = build_record(variant_mapping(), variant_document("Default Title"))
        self.assertEqual(record["Name"], "The Hidden Snowboard")

    def test_an_untitled_variant_is_named_after_the_product(self):
        record = build_record(variant_mapping(), variant_document(""))
        self.assertEqual(record["Name"], "The Hidden Snowboard")

    def test_a_missing_product_does_not_raise(self):
        document = variant_document()
        document.pop("product")
        record = build_record(variant_mapping(), document)
        self.assertEqual(record["Shopify_Variant_Id__c"], "555")


if __name__ == "__main__":
    unittest.main()
