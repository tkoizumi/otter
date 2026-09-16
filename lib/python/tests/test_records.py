"""Tests for the record-building mechanics."""

import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))

from otter_connectors.records import (  # noqa: E402
    build_record,
    effective_limits,
    joined,
    omit_empty,
    resolve,
    text,
    truncate,
    validate_mapping,
)


class Sized(str):
    """Stands in for a schema reference: a string that knows its length.

    Deliberately *not* an ``otter_schema.Field``. The convention has to work for
    any str-like key, which is what keeps ``records`` from depending on the
    schema package.
    """

    def __new__(cls, name, length=None):
        self = super().__new__(cls, name)
        self.length = length
        return self


class ResolveTests(unittest.TestCase):
    SOURCE = {"a": {"b": {"c": 3}}, "flat": "value", "none": None, "list": [1, 2]}

    def test_walks_a_dotted_path(self):
        self.assertEqual(resolve(self.SOURCE, "a.b.c"), 3)
        self.assertEqual(resolve(self.SOURCE, "flat"), "value")

    def test_a_missing_link_is_not_an_error(self):
        # Optional source fields are the norm; an absent value means "omit this
        # field", not a crash.
        for path in ("a.missing.c", "nothing.at.all", "a.b.c.deeper", "none.deeper"):
            self.assertEqual(resolve(self.SOURCE, path), "", path)

    def test_a_non_mapping_in_the_middle_stops_the_walk(self):
        self.assertEqual(resolve(self.SOURCE, "list.0"), "")

    def test_empty_path(self):
        self.assertEqual(resolve(self.SOURCE, ""), "")


class TextTests(unittest.TestCase):
    def test_none_becomes_empty(self):
        self.assertEqual(text(None), "")

    def test_strips_whitespace(self):
        self.assertEqual(text("  padded \n"), "padded")

    def test_coerces_non_strings(self):
        self.assertEqual(text(42), "42")


class JoinedTests(unittest.TestCase):
    def test_joins_present_paths_in_order(self):
        source = {"addr": {"line1": "1 Main St", "line2": "Apt 4"}}
        self.assertEqual(joined("addr.line1", "addr.line2")(source), "1 Main St\nApt 4")

    def test_skips_empty_and_missing_parts(self):
        source = {"addr": {"line1": "1 Main St", "line2": "   "}}
        self.assertEqual(joined("addr.line1", "addr.line2")(source), "1 Main St")
        self.assertEqual(joined("addr.nope", "addr.line2")(source), "")

    def test_honours_a_custom_separator(self):
        source = {"a": "x", "b": "y"}
        self.assertEqual(joined("a", "b", separator=", ")(source), "x, y")

    def test_rejects_unknown_options(self):
        with self.assertRaises(TypeError):
            joined("a", seperator=", ")


class OmitEmptyTests(unittest.TestCase):
    def test_drops_blank_but_keeps_zero_and_false(self):
        record = {"a": "x", "b": "", "c": None, "d": 0, "e": False}
        self.assertEqual(omit_empty(record), {"a": "x", "d": 0, "e": False})


class TruncateTests(unittest.TestCase):
    def test_truncates_to_the_field_limit(self):
        self.assertEqual(truncate({"a": "abcdef"}, {"a": 3}), {"a": "abc"})

    def test_leaves_fields_without_a_limit(self):
        self.assertEqual(truncate({"b": "abcdef"}, {"a": 3}), {"b": "abcdef"})

    def test_leaves_non_strings_alone(self):
        self.assertEqual(truncate({"a": 12345}, {"a": 2}), {"a": 12345})


class ValidateMappingTests(unittest.TestCase):
    def test_accepts_every_supported_form(self):
        mapping = {
            "a": "some.path",
            "b": ("other.path", text),
            "c": lambda source: source.get("x"),
        }
        self.assertIs(validate_mapping(mapping), mapping)

    def test_rejects_junk_with_the_target_field_named(self):
        for spec in ("", 7, ("path",), ("path", "not callable"), {"path": "x"}):
            with self.assertRaises(TypeError) as caught:
                validate_mapping({"Target__c": spec})
            self.assertIn("Target__c", str(caught.exception), spec)


class BuildRecordTests(unittest.TestCase):
    SOURCE = {
        "id": "gid://shopify/Customer/99",
        "firstName": "  Ann  ",
        "lastName": None,
        "email": "",
        "address": {"city": "Springfield", "country": "United States"},
    }

    def test_assembles_from_a_mixed_mapping(self):
        mapping = {
            "External__c": lambda source: source["id"].rsplit("/", 1)[-1],
            "FirstName": "firstName",
            "Email": "email",
            "City": "address.city",
            "Label": ("address.city", lambda value: value.upper()),
        }

        record = build_record(mapping, self.SOURCE)

        self.assertEqual(record, {
            "External__c": "99",
            "FirstName": "Ann",
            "City": "Springfield",
            "Label": "SPRINGFIELD",
        })

    def test_omits_fields_with_no_source_value(self):
        record = build_record({"Email": "email", "Missing": "nope.deep"}, self.SOURCE)
        self.assertEqual(record, {})

    def test_applies_field_limits(self):
        mapping = {"City": ("address.city", lambda value: value + " Illinois")}
        record = build_record(mapping, self.SOURCE, limits={"City": 7})
        self.assertEqual(record, {"City": "Springf"})

    def test_a_target_key_can_carry_its_own_length(self):
        """No table, no `limits` argument: the mapping key knows."""
        mapping = {Sized("City", 7): ("address.city", lambda v: v + " Illinois")}
        self.assertEqual(build_record(mapping, self.SOURCE), {"City": "Springf"})

    def test_an_explicit_limit_beats_the_key(self):
        mapping = {Sized("City", 7): "address.city"}
        record = build_record(mapping, self.SOURCE, limits={Sized("City", 7): 3})
        self.assertEqual(record, {"City": "Spr"})

    def test_a_none_limit_opts_the_field_out(self):
        mapping = {Sized("City", 7): "address.city"}
        record = build_record(mapping, self.SOURCE, limits={Sized("City", 7): None})
        self.assertEqual(record, {"City": "Springfield"})

    def test_a_key_without_a_length_is_never_truncated(self):
        mapping = {Sized("City"): "address.city"}
        self.assertEqual(build_record(mapping, self.SOURCE), {"City": "Springfield"})

    def test_a_plain_string_mapping_is_untouched(self):
        """The hand-written case: no lengths anywhere, so nothing truncates."""
        record = build_record({"City": "address.city"}, self.SOURCE)
        self.assertEqual(record, {"City": "Springfield"})

    def test_non_string_values_are_never_truncated(self):
        mapping = {Sized("Count", 1): lambda _src: 12345}
        self.assertEqual(build_record(mapping, self.SOURCE), {"Count": 12345})

    def test_effective_limits_is_empty_for_a_plain_mapping(self):
        self.assertEqual(effective_limits({"City": "address.city"}), {})
        self.assertEqual(effective_limits({"City": "address.city"}, None), {})

    def test_effective_limits_keeps_an_explicit_table(self):
        self.assertEqual(effective_limits({"City": "a"}, {"City": 4}), {"City": 4})

    def test_a_transform_runs_only_when_the_path_resolves(self):
        calls = []

        def transform(value):
            calls.append(value)
            return value

        build_record({"Missing": ("nope.deep", transform)}, self.SOURCE)
        self.assertEqual(calls, [], "a transform should not run for a missing path")

    def test_rejects_an_unsupported_spec(self):
        with self.assertRaises(TypeError):
            build_record({"Bad__c": 7}, self.SOURCE)


if __name__ == "__main__":
    unittest.main()
