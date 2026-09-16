"""Turning a source document into an API payload, without hiding the mapping.

An integration's field mapping is *intent*: which source field lands in which
target field. Written as one long function that intent gets buried under
mechanics -- stripping whitespace, dropping empties, truncating to field
lengths -- and under the handful of fields that genuinely need logic.

This module takes the mechanics, so an integration can express the mapping as
data::

    mapping = {
        "Shopify_Customer_Id__c": numeric_id,          # callable: needs logic
        "LastName": last_name,                         # callable
        "FirstName": "firstName",                      # dotted source path
        "MailingStreet": joined("defaultAddress.address1",
                                "defaultAddress.address2"),
        "Email": ("email", text),                      # path + transform
    }
    record = build_record(mapping, customer, limits=MAX_FIELD_LENGTH)

The grammar is deliberately tiny, and a **callable is always a legal value**.
That escape hatch is the important part: without it, every new integration
would need a new operator in a declarative language, which is how mapping files
turn into programming languages. Here, anything that does not fit is just
Python -- testable, debuggable and importable.

One convention reaches beyond that grammar: **a target key exposing ``length``
truncates to it**, so a mapping built from schema references carries its own
field lengths and needs no separate table::

    mapping = {Product2.Name: "title", Product2.ProductCode: "sku"}
    record = build_record(mapping, variant)      # lengths come from the keys

A mapping of plain strings is unaffected -- a string has no ``length``, so
nothing is truncated unless ``limits`` is passed, exactly as before. That is
what keeps this module usable by an integration that hand-codes its field names.

Nothing in this module knows about Shopify, Salesforce or any other product.
"""

__all__ = [
    "build_record",
    "effective_limits",
    "joined",
    "omit_empty",
    "resolve",
    "text",
    "truncate",
    "validate_mapping",
]


def text(value):
    """Coerce a value to a stripped string, with ``None`` becoming ``""``."""
    if value is None:
        return ""
    return str(value).strip()


def resolve(source, path):
    """Look up a dotted path, returning ``""`` when any link is missing.

    Missing is not an error: sources have optional fields, and an absent value
    should mean "do not send this field", not a crash.
    """
    if not path:
        return ""
    current = source
    for part in path.split("."):
        if not isinstance(current, dict):
            return ""
        current = current.get(part)
        if current is None:
            return ""
    return current


def joined(*paths, **options):
    """A mapping entry joining several paths, skipping the empty ones.

    Returns a callable, so it drops straight into a mapping::

        "MailingStreet": joined("defaultAddress.address1", "defaultAddress.address2")
    """
    separator = options.pop("separator", "\n")
    if options:
        raise TypeError("joined() got unexpected options: %s" % ", ".join(sorted(options)))

    def extract(source):
        return separator.join(
            part for part in (text(resolve(source, path)) for path in paths) if part
        )

    return extract


def omit_empty(record):
    """Drop fields whose value is ``None`` or blank.

    A sync should not blank a field it has no data for, so omission is safer
    than sending null.
    """
    return {key: value for key, value in record.items() if value not in (None, "")}


def truncate(record, limits):
    """Truncate string values to per-field limits.

    APIs tend to truncate silently or reject the whole record, so doing it here
    keeps a long value from costing you the record.
    """
    out = {}
    for key, value in record.items():
        limit = limits.get(key)
        if limit and isinstance(value, str) and len(value) > limit:
            value = value[:limit]
        out[key] = value
    return out


def effective_limits(mapping, limits=None):
    """The truncation limits to apply: an explicit table, then each target
    key's own declared length.

    A schema reference carries the length its target system declared for it, so
    a mapping built from references needs no separate table. A mapping of plain
    strings is untouched -- a string has no ``length``, so the result is empty
    and nothing is truncated, exactly as before.

    An explicit entry in ``limits`` always wins, including ``None``, which opts
    a field out of truncation.
    """
    out = dict(limits) if limits else {}
    for key in mapping:
        if key in out:
            continue
        length = getattr(key, "length", None)
        if length:
            out[key] = length
    return out


def validate_mapping(mapping):
    """Check a mapping's shape, raising ``TypeError`` on the first bad entry.

    Called before any records are processed so a typo fails immediately with a
    named field, rather than on some later page -- or never, if that page turns
    out to be empty.

    Where the mapping's targets know which object they belong to -- schema
    references do -- it also rejects a mapping that spans more than one. An
    upsert writes to a single object, so mixing them is always a mistake, and
    one that would otherwise fail per record.
    """
    for target, spec in mapping.items():
        if callable(spec):
            continue
        if isinstance(spec, str):
            if not spec:
                raise TypeError("mapping for %r is an empty path" % target)
            continue
        if isinstance(spec, tuple):
            if len(spec) != 2 or not isinstance(spec[0], str) or not callable(spec[1]):
                raise TypeError(
                    "mapping for %r must be (path, transform), got %r" % (target, spec))
            continue
        raise TypeError(
            "mapping for %r must be a source path, a (path, transform) pair or a "
            "callable, got %r" % (target, spec))

    # Only meaningful for targets that name their object; a hand-written
    # mapping has none, so the set is empty and this does nothing.
    objects = {getattr(key, "object_name", None) for key in mapping}
    objects.discard(None)
    if len(objects) > 1:
        raise TypeError(
            "a mapping must target one object, got %s" % ", ".join(sorted(objects)))
    return mapping


def build_record(mapping, source, limits=None):
    """Render ``source`` into a record according to ``mapping``.

    Each value is a source path (``"a.b.c"``), a ``(path, transform)`` pair, or
    a callable taking the whole source. A transform runs only when its path
    resolved to something, so it can assume a real value. String results are
    stripped, empty fields are omitted, and the result is truncated to the
    target's field lengths.

    Lengths come from ``limits`` when given, and otherwise from the mapping's
    own keys, so a schema-referenced mapping needs no table::

        build_record(mapping, variant)                 # lengths from the keys
        build_record(mapping, variant, limits=table)   # explicit table wins
    """
    record = {}
    for target, spec in mapping.items():
        if callable(spec):
            value = spec(source)
        elif isinstance(spec, str):
            value = resolve(source, spec)
        elif isinstance(spec, tuple) and len(spec) == 2:
            path, transform = spec
            raw = resolve(source, path)
            if raw in (None, ""):
                # Nothing to transform. Skipping means a transform can assume it
                # is given a value, instead of every one having to guard for "".
                continue
            value = transform(raw)
        else:
            raise TypeError(
                "mapping for %r must be a source path, a (path, transform) pair or a "
                "callable, got %r" % (target, spec))

        if isinstance(value, str):
            value = value.strip()
        if value in (None, ""):
            continue
        record[target] = value

    effective = effective_limits(mapping, limits)
    if effective:
        record = truncate(record, effective)
    return record
