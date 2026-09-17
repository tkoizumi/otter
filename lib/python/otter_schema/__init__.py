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

__all__ = ["Field", "Node", "SchemaError"]


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

    def __get__(self, instance, owner=None):
        """Make a bare class attribute work in a nested position.

        Accessed on the class -- ``Product2.Name`` -- a Field is just itself, so
        a flat schema needs nothing special. Accessed on a ``Node`` instance --
        ``ProductVariant.product.title`` -- the same attribute has to resolve to
        ``"product.title"``, because that is the path ``resolve`` looks up. The
        instance is what knows the prefix, so this returns a copy carrying it.

        That makes the two systems' generated code the same shape: one line per
        field, ``title = Field("title", ...)``, in both.
        """
        if instance is None:
            return self
        path = instance.at(str(self))
        if path == str(self):
            return self
        return Field(
            path, object_name=self.object_name, type=self.type, length=self.length,
            external_id=self.external_id, unique=self.unique, required=self.required,
            read_only=self.read_only, picklist=self.picklist,
            reference_to=self.reference_to)

    def __repr__(self):
        return "Field(%s.%s)" % (self.object_name, str(self))


class Node:
    """A type proxy whose attribute access builds a dotted path.

    Generated Shopify types are instances of a ``Node`` subclass, and every
    field is a property that prepends the instance's path::

        ProductVariant.product.title    # -> Field("product.title")
        Product.title                   # -> Field("title")

    A plain class attribute cannot do this. ``ProductVariant.product`` would
    evaluate to the class, and ``.title`` on a class is just its own attribute --
    the ``product.`` prefix is lost, and ``resolve`` then looks in the wrong
    place and finds nothing. The prefix has to live somewhere, and a class
    attribute has no state.

    It is worth the machinery because a nested reference is still a string, so
    it drops into the mapping grammar exactly like a flat one.
    """

    def __init__(self, path=""):
        self.path = path

    def at(self, field):
        """The dotted path to one field, relative to the document root."""
        return field if not self.path else self.path + "." + field

    def __getattr__(self, field):
        # Only reached when normal lookup fails, which is exactly the case
        # worth naming well: the generated class is `_ProductVariant` and
        # leaking that underscore to a developer helps nobody.
        raise AttributeError(
            "%s has no field %r in the pulled schema"
            % (type(self).__name__.lstrip("_"), field))


