"""Pull Shopify types and render them as schema modules.

Shopify is a **typed graph**, not a flat field list like Salesforce, and that
changes two things.

**A field reference carries a path, not just a name.** The mapping hands
``build_record`` the response for one node, and ``resolve`` looks up a dotted
path in it. So ``ProductVariant.product.title`` has to evaluate to the string
``"product.title"`` -- which a plain class attribute cannot do, because a class
attribute has no state and the ``product.`` prefix is lost the moment you chain.
The generated types are therefore *instances* whose object fields are properties
that extend a path prefix (see :class:`otter_schema.Node`).

**Not every field is a value.** A connection (``variants``, ``metafields``)
needs ``first``/``after`` and wraps its results in ``nodes``; a list is a list.
``resolve`` walks dicts and cannot index either, so neither can appear in a
mapping path. The walk skips both, which is what keeps the generated surface
small and honest: every symbol it emits is something a mapping can actually use.

Introspection needs no data scopes -- it describes the schema, and scopes gate
data -- so this works on an app that cannot yet read a single product.
"""

from . import SchemaError

__all__ = [
    "Arg", "Connection", "collect", "enum_values",
    "fetch_introspection", "find_root_connections", "introspection_query",
    "is_connection", "render_root", "render_sdl", "sdl_header",
    "render_types", "unwrap",
]

#: Nesting depth for the ofType wrappers. NON_NULL(LIST(NON_NULL(X))) is four
#: levels, so five leaves room to see the named type and whether it is a list.
_OF_TYPE = "kind name ofType { " * 5 + "kind name " + "}" * 5

_INTROSPECT = """query($name: String!) {
  __type(name: $name) {
    kind
    name
    fields(includeDeprecated: false) {
      name
      description
      type { %s }
    }
  }
}""" % _OF_TYPE

#: Types that are objects by kind but never a mapping path.
_SCALARS = {
    "String", "Int", "Float", "Boolean", "ID", "DateTime", "Date", "URL",
    "Decimal", "Money", "JSON", "HTML", "UnsignedInt64", "BigInt", "FormattedString",
}


def introspection_query():
    return _INTROSPECT


def unwrap(type_ref):
    """Reduce a GraphQL type reference to ``(kind, name, is_list, non_null)``.

    The wrappers nest: ``[SelectedOption!]!`` is
    ``NON_NULL(LIST(NON_NULL(SelectedOption)))``.
    """
    kind = name = None
    is_list = non_null = False
    current = type_ref or {}
    while current:
        wrapper = current.get("kind")
        if wrapper == "NON_NULL":
            non_null = True
        elif wrapper == "LIST":
            is_list = True
        elif wrapper in ("OBJECT", "SCALAR", "ENUM", "INTERFACE", "UNION", "INPUT_OBJECT"):
            kind, name = wrapper, current.get("name")
            break
        current = current.get("ofType")
    return kind, name, is_list, non_null


def is_connection(name, kind):
    """Whether a field is a Relay connection rather than a value.

    Shopify names every connection ``<Something>Connection``, and a connection
    is an object kind, so the name is what separates "a thing a record can read
    from" (``inventoryItem``) from "a page of things" (``metafields``).
    """
    return kind == "OBJECT" and bool(name) and name.endswith("Connection")


def followable(type_ref):
    """The named object type a mapping path can walk into, or ``None``.

    Singular objects only: a list cannot be indexed by ``resolve``, and a
    connection additionally needs paging arguments.
    """
    kind, name, is_list, _ = unwrap(type_ref)
    if is_list or kind != "OBJECT" or name in _SCALARS:
        return None
    if is_connection(name, kind):
        return None
    return name


def collect(client, root, depth=2, limit=200):
    """Walk the graph from ``root`` and return ``{type name: [field, ...]}``.

    ``depth`` counts hops of singular object fields from the root, so the
    default of 2 covers ``ProductVariant.product.title`` and
    ``ProductVariant.product.featuredImage.url`` but stops there. Raise it for
    deeper paths; every extra hop pulls in more types.
    """
    types = {}
    queue = [(root, 0)]
    while queue:
        name, level = queue.pop(0)
        if name in types or len(types) >= limit:
            continue
        data = client.graphql(introspection_query(), {"name": name})
        described = data.get("__type") or {}
        if described.get("kind") != "OBJECT":
            # An enum or scalar reached as a root: nothing to render, but not an
            # error worth failing the pull over.
            types[name] = []
            continue
        fields = described.get("fields") or []
        types[name] = fields
        if level >= depth:
            continue
        for field in fields:
            child = followable(field.get("type"))
            if child and child not in types:
                queue.append((child, level + 1))
    return types


def module_name(type_name):
    """``ProductVariant`` -> ``product_variant``."""
    out = []
    for index, char in enumerate(type_name):
        if char.isupper() and index and not type_name[index - 1].isupper():
            out.append("_")
        out.append(char.lower())
    return "".join(out)


def render_types(root, types, *, integration, source_url, api_version, fetched_at,
                 connections=None):
    """Render a closure as one importable module."""
    ordered = [root] + sorted(name for name in types if name != root)
    lines = [
        '"""Generated by `make sync-schema`. Do not edit by hand.',
        "",
        "System:       shopify",
        "Root:         %s" % root,
        "Source:       %s" % source_url,
        "API version:  %s" % api_version,
        "Fetched:      %s" % fetched_at,
        "Types:        %d" % len(ordered),
        "",
        "Regenerate with::",
        "",
        "    make sync-schema INTEGRATION=%s SYSTEM=shopify OBJECT=%s" % (integration, root),
        "",
        "Every attribute is a ``Field``, which is a ``str`` carrying the dotted",
        "path it resolves to. Nested types are reached by attribute:",
        "``%s.<object field>.<field>``." % root,
        '"""',
        "",
        "from otter_schema import Field, Node",
    ]
    if connections:
        lines.append("from otter_schema.shopify import Arg, Connection")
    lines.extend([
        "",
        "__all__ = [",
    ])
    for name in ordered:
        lines.append('    "%s",' % name)
    lines.append("]")
    lines.append("")

    for name in ordered:
        lines.extend(_render_class(name, types.get(name) or []))
        lines.append("")

    for name in ordered:
        if connections and name in connections:
            lines.extend(render_root("_%s" % name, connections[name]))

    # Instances, not classes: `ProductVariant.product.title` has to build a
    # path, and that needs somewhere for the prefix to live.
    for name in ordered:
        lines.append("%s = _%s()" % (name, name))
    return "\n".join(lines).rstrip("\n") + "\n"


def _render_class(name, fields):
    lines = ["", "class _%s(Node):" % name, '    """%s."""' % name, ""]
    rendered = 0
    for field in sorted(fields, key=lambda f: f.get("name") or ""):
        field_name = field.get("name")
        if not field_name or field_name.startswith("__"):
            continue
        kind, named, is_list, non_null = unwrap(field.get("type"))
        child = followable(field.get("type"))
        if child:
            if lines and lines[-1].strip():
                lines.append("")
            lines.append("    @property")
            lines.append("    def %s(self) -> \"_%s\":" % (field_name, child))
            lines.append("        return _%s(self.at(\"%s\"))" % (child, field_name))
            lines.append("")
        elif kind == "OBJECT" or is_list or kind in ("INTERFACE", "UNION"):
            # A connection or a list. Neither can appear in a mapping path --
            # `resolve` walks dicts and cannot index -- so emitting a symbol for
            # one would promise a reference that silently resolves to nothing.
            continue
        else:
            # A plain class attribute, exactly as the Salesforce generator emits
            # it. Field is a descriptor, so the same line resolves correctly
            # whether it is read off the root instance or through a nested one.
            lines.append("    %s = Field(%s, object_name=%s, type=%s, required=%s)"
                         % (field_name, _quote(field_name), _quote(name),
                            _quote(str(named or kind)),
                            "True" if non_null else "False"))
        rendered += 1
    if not rendered:
        lines.append("    pass")
    return lines


def _quote(value):
    return '"%s"' % value


# -- root connections and query building ------------------------------------ #
#
# A mapping reads one node, but a *query* fetches a page of them, and only the
# schema knows which connection a type is reached through. The generator records
# it, so building a document needs no network and no schema at runtime.

#: The QueryRoot fields, with the argument metadata a builder needs. Introspected
#: once per pull and cached for the run.
_QUERY_ROOT = """query {
  __type(name: "QueryRoot") {
    fields {
      name
      args { name type { %s } }
      type { %s }
    }
  }
}""" % (_OF_TYPE, _OF_TYPE)

_ENUM_VALUES = """query($name: String!) {
  __type(name: $name) { enumValues { name } }
}"""


class Arg:
    """One argument of a connection field."""

    def __init__(self, type, required=False, values=None):
        self.type = type
        self.required = required
        #: Legal values when the argument is an enum, so a bad sort key is an
        #: error here rather than a GraphQL validation error at run time.
        self.values = tuple(values) if values else None


class Connection:
    """The root connection a type is reached through."""

    def __init__(self, name, args=None):
        self.name = name
        self.args = dict(args or {})


def enum_values(client, type_name):
    data = client.graphql(_ENUM_VALUES, {"name": type_name})
    entries = (data.get("__type") or {}).get("enumValues") or []
    return tuple(entry.get("name") for entry in entries if entry.get("name"))


def find_root_connections(client, wanted):
    """``{type name: Connection}`` for every type in ``wanted`` a query can start from.

    Found by the node type of the connection a ``QueryRoot`` field returns, not
    by guessing a plural: ``ProductVariant`` -> ``productVariants`` is a
    convention, not a rule, and ``SEO`` would defeat any attempt to derive it.

    One introspection call covers the whole closure, which matters because
    ``Product`` is reachable from ``ProductVariant`` and a developer may well
    want to query either. Attaching a connection only to the type that happened
    to be the pull root would leave the other one unusable.
    """
    data = client.graphql(_QUERY_ROOT)
    found = {}
    for field in (data.get("__type") or {}).get("fields") or []:
        kind, name, _, _ = unwrap(field.get("type"))
        if kind != "OBJECT" or not name or not name.endswith("Connection"):
            continue
        node = name[: -len("Connection")]
        if node not in wanted or node in found:
            continue
        args = {}
        for entry in field.get("args") or []:
            arg_kind, arg_name, _, required = unwrap(entry.get("type"))
            args[entry.get("name")] = Arg(
                arg_name, required=required,
                values=enum_values(client, arg_name) if arg_kind == "ENUM" else None)
        found[node] = Connection(field.get("name"), args)
    return found


def render_root(class_name, connection):
    """The generated ``_Root.ROOT = Connection(...)`` block.

    Records the connection a type is reached through, with the arguments it
    takes and the values an enum argument accepts. Nothing reads it at run time
    any more -- the query is a ``.graphql`` document -- but it is what you write
    that document against, so the root name and its paging arguments come from
    the pulled schema rather than from memory.
    """
    lines = ["", "%s.ROOT = Connection(" % class_name, "    %r," % connection.name, "    {"]
    for name in sorted(connection.args):
        arg = connection.args[name]
        parts = ["%r" % arg.type]
        if arg.required:
            parts.append("required=True")
        if arg.values:
            parts.append("values=(" + ", ".join("%r" % v for v in arg.values) + ",)")
        lines.append("        %r: Arg(%s)," % (name, ", ".join(parts)))
    lines.extend(["    },", ")", ""])
    return lines


# -- SDL, for editor tooling ------------------------------------------------ #
#
# The Python symbols above serve the mapping: they are paths and metadata. An
# editor needs something else -- a schema it can validate documents against --
# and that is SDL. Shopify serves the whole schema introspectively, so this is a
# conversion rather than a walk: no closure to choose, and nothing a document
# might reference is missing.
#
# It is deliberately the *whole* schema. A trimmed SDL fails validation on
# anything left out, so a query using a type outside the trim reports an error
# that is not real -- the worst property for a validation aid.

#: Nesting depth for ofType wrappers. Several Shopify types are decorated deeply
#: enough that the usual four levels fail with "Decorated type deeper than
#: introspection query".
_SDL_OF_TYPE = "kind name ofType { " * 8 + "kind name " + "}" * 8

_INTROSPECTION = """query IntrospectSchema {
  __schema {
    queryType { name }
    mutationType { name }
    directives {
      name description isRepeatable locations
      args { name description defaultValue type { %s } }
    }
    types {
      kind name description
      fields(includeDeprecated: true) {
        name description isDeprecated deprecationReason
        args { name description defaultValue type { %s } }
        type { %s }
      }
      inputFields { name description defaultValue type { %s } }
      interfaces { name }
      enumValues(includeDeprecated: true) {
        name description isDeprecated deprecationReason
      }
      possibleTypes { name }
    }
  }
}""" % ((_SDL_OF_TYPE,) * 4)


def fetch_introspection(client):
    """The whole GraphQL schema, as introspection JSON."""
    data = client.graphql(_INTROSPECTION)
    schema = data.get("__schema")
    if not schema:
        raise SchemaError("Shopify returned no __schema for introspection")
    return schema


def render_sdl(schema, *, integration, source_url, api_version, fetched_at):
    """SDL for the whole schema, so an editor can validate documents offline.

    Needs ``graphql-core``, which is a development dependency: it converts
    introspection to SDL and validates documents, and neither belongs in a
    prepared runtime environment.
    """
    try:
        from graphql import build_client_schema, print_schema
    except ImportError as exc:  # pragma: no cover - exercised by the message
        raise SchemaError(
            "SDL output needs graphql-core. Install it with:\n"
            "    python3 -m pip install graphql-core") from exc

    body = print_schema(build_client_schema({"__schema": schema}))
    return sdl_header(integration=integration, source_url=source_url,
                      api_version=api_version, fetched_at=fetched_at) + body


def sdl_header(*, integration, source_url, api_version, fetched_at):
    """The provenance block at the top of the generated SDL.

    Separate from :func:`render_sdl` so it can be checked without graphql-core
    installed, which is the usual case -- it is a development extra.
    """
    return "\n".join([
        "# Generated by `make sync-schema`. Do not edit by hand.",
        "#",
        "# System:       shopify",
        "# Source:       %s" % source_url,
        "# API version:  %s" % api_version,
        "# Fetched:      %s" % fetched_at,
        "#",
        "# Regenerate with::",
        "#",
        "#     make sync-schema INTEGRATION=%s SYSTEM=shopify OBJECT=ProductVariant"
        % integration,
        "#",
        "# This is the schema an editor validates `.graphql` documents against,",
        "# wired up by graphql.config.yml at the repository root. It is the whole",
        "# schema on purpose: a trimmed one reports errors that are not real.",
        "",
    ]) + "\n"
