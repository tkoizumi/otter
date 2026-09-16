"""Schema references for integration mappings.

An integration's mapping names fields on two systems. Written as bare strings
those names are unchecked: a typo is legal Python, survives every test, and
surfaces as a per-record rejection inside a run that still reports ``succeeded``.

This package replaces the strings with references into a schema that was pulled
from the system itself, so the name is wrong at *import* time or not at all.

A reference **is a string**::

    class Product2:
        Name = Field("Name", object_name="Product2", type="string", length=80)

That matters more than it looks. ``otter_connectors.records`` already accepts a
string as a mapping key, as a source path, and as the first half of a
``(path, transform)`` pair, so a ``Field`` drops into the existing grammar with
no change to the mapping engine at all -- and therefore no re-release of every
integration that shares it.

The metadata is what a mapping would otherwise restate by hand. ``length`` is
read by ``otter_connectors.records.build_record`` directly off the mapping's
keys, so there is no truncation table to keep in step; ``external_id`` and
``unique`` let an integration assert its own upsert key before the first record;
``picklist`` turns "will the org accept this value?" into a build-time question.
"""

__all__ = ["Field", "SchemaError"]


class SchemaError(Exception):
    """A schema reference does not name something the system has."""


class Field(str):
    """A field name that knows what it is.

    It compares and hashes as the plain name, so it is interchangeable with a
    string everywhere the mapping grammar expects one.
    """

    def __new__(cls, name, *, object_name, type, length=None, external_id=False,
                unique=False, required=False, read_only=False,
                picklist=None, reference_to=None):
        self = super().__new__(cls, name)
        self.object_name = object_name
        self.type = type
        # Salesforce reports length 0 for anything that is not a string; None
        # reads better than a misleading zero.
        self.length = length or None
        self.external_id = external_id
        self.unique = unique
        # Required at create time: the org insists on a value and will not
        # supply one itself.
        self.required = required
        # Not createable. Mapping one is a mistake the org will reject per
        # record, which is the failure this package exists to prevent.
        self.read_only = read_only
        # None means "not a picklist, or its values were not recorded".
        # type == "picklist" is the reliable signal that it is one.
        self.picklist = tuple(picklist) if picklist else None
        self.reference_to = tuple(reference_to) if reference_to else None
        return self

    @property
    def is_picklist(self):
        return self.type == "picklist"

    def __repr__(self):
        return "Field(%s.%s)" % (self.object_name, str(self))

