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

Nothing in this module knows about Shopify, Salesforce or any other product.
"""

__all__ = [
    "build_record",
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


def validate_mapping(mapping):
    """Check a mapping's shape, raising ``TypeError`` on the first bad entry.

    Called before any records are processed so a typo fails immediately with a
    named field, rather than on some later page -- or never, if that page turns
    out to be empty.
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
    return mapping


def build_record(mapping, source, limits=None):
    """Render ``source`` into a record according to ``mapping``.

    Each value is a source path (``"a.b.c"``), a ``(path, transform)`` pair, or
    a callable taking the whole source. A transform runs only when its path
    resolved to something, so it can assume a real value. String results are
    stripped, empty fields are omitted, and ``limits`` truncates by length.
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

    if limits:
        record = truncate(record, limits)
    return record
