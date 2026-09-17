# otter_schema

Schema references for integration mappings.

An integration's mapping names fields on two systems. Written as bare strings,
those names are unchecked: `"ProductName"` is legal Python, passes every test,
and is rejected by Salesforce one record at a time inside a run that still
reports `succeeded`. Nothing tells you.

This package replaces the strings with references into a schema pulled from the
system itself. A wrong name then fails at **import**, before a run exists.

```python
from schema.salesforce import Product2

mapping = {
    Product2.Name:                  "title",
    Product2.ProductCode:           "sku",
    Product2.Shopify_Variant_Id__c: "id",
}

record = build_record(mapping, variant)   # lengths come from the keys
```

There is no truncation table to keep in step. ``build_record`` reads each
target's declared length off the mapping key itself, so ``Product2.Name``
truncates to the org's 255 without anyone restating it.

## A reference *is* a string

`Field` subclasses `str`. That is the whole design, and it means
`otter_connectors.records` needs no change at all: the mapping grammar already
accepts a string as a key, as a source path, and as the first half of a
`(path, transform)` pair. So a schema reference drops into an existing
integration with no re-release of the mapping engine that every other
integration shares.

The metadata is what a mapping would otherwise restate by hand:

| Attribute | Replaces |
| --- | --- |
| `length` | a hand-maintained `MAX_FIELD_LENGTH` table; read directly by `build_record` |
| `external_id`, `unique` | "is my upsert key actually an upsert key?" |
| `required` | "will the org reject an empty value?" |
| `read_only` | "can I write this at all?" |
| `picklist` | "will the org accept this value?" |
| `reference_to` | which object a lookup points at |

## Pulling a schema

From the repository root:

```sh
make sync-schema INTEGRATION=shopify-product-to-salesforce-product
```

That is the whole command. Everything else is discovered: the object name,
instance URL and API version come from the integration's `otter.yaml`, and the
credentials from `otter.env` at the checkout root. Precedence is flag, then an
explicit environment variable, then the env file, then the manifest -- so a
one-off retarget needs no edit:

```sh
SALESFORCE_OBJECT=Contact make sync-schema INTEGRATION=shopify-to-salesforce
make sync-schema INTEGRATION=x SYSTEM=salesforce OBJECT="Contact Shopify_Order__c"
```

`SYSTEM` selects which system to pull from and writes to `schema/<SYSTEM>/`.
`salesforce` and `shopify` are implemented. `OBJECT` takes several, space- or
comma-separated.

Equivalent without make -- no `PYTHONPATH`, no sourcing:

```sh
python3 lib/python/otter_schema/pull.py \
    --integration integrations/shopify-product-to-salesforce-product
```

Output lands in `<integration>/schema/salesforce/` and is meant to be
**committed**: it is part of the artifact, and a developer should be able to read
and autocomplete it without network access.

| Flag | Meaning |
| --- | --- |
| `--integration DIR` | the integration directory, or a bare name that resolves under `<checkout>/integrations/` (default: the current one) |
| `--system NAME` | which system to pull from (default: `salesforce`); output goes to `schema/<NAME>/` |
| `--object NAME` | sObject API name; repeatable. Default: `SALESFORCE_OBJECT` |
| `--env-file PATH` | shared credentials (default: `<checkout>/otter.env`) |
| `--out DIR` | output directory. Default: `<integration>/schema/salesforce` |
| `--instance-url URL` | default: the manifest, else `$SALESFORCE_INSTANCE_URL` |
| `--api-version V` | default: the manifest, else `$SALESFORCE_API_VERSION`, else `62.0` |
| `--picklists` | embed each picklist's active values (off by default: a country picklist is hundreds of lines) |
| `--depth` | how many object hops to follow from the root (default: 2) |
| `--no-sdl` | skip the SDL file; only the Python module is written |
| `--dry-run` | print the module, write nothing |

Pulling one object never drops another: the package index is rebuilt by scanning
the directory.

## The caveat that matters

**A stored schema is a second source of truth, and a stale one is worse than
none** — it validates against an org that no longer exists, and does so
confidently.

The mitigation is not built yet: a `--check` that re-describes and diffs, to run
in CI or before a deploy. Until then, treat `schema/` as a snapshot with a
`Fetched:` timestamp on it and re-pull when you touch an integration. The
snapshot is still strictly better than hand-typed names, which are stale the
moment someone edits the org and wrong from the start when a name is a typo.

## Shopify

`make sync-schema SYSTEM=shopify OBJECT=ProductVariant` walks Shopify's type
graph from a root and writes one module per root. Two differences from the flat
Salesforce case, both forced by the schema:

- **Only the `kind` decides what is a field.** `inventoryPolicy` resolves to
  `ProductVariantInventoryPolicy`, an enum; `legacyResourceId` to
  `UnsignedInt64`, a scalar. Both look like object types by name.
- **Connections and lists are skipped, not emitted.** A connection needs
  `first`/`after` and wraps its results in `nodes`; a list is a list. `resolve`
  walks dicts and cannot index either, so a symbol for one would promise a
  reference that silently resolves to nothing.

Nested types chain, because a field reference has to carry a *path* and not just
a name:

```python
from schema.shopify import ProductVariant
ProductVariant.product.title        # Field("product.title")
```

That works because `Field` is a descriptor: read off a class it is itself, so a
flat schema like Salesforce is unaffected, and read off a nested `Node` it
returns a copy carrying the prefix. `--depth` (default 2) bounds how many hops
are followed.

Every generated type also carries a `ROOT` marker — `Product.ROOT` names the
connection it is reached through, with the arguments that connection takes and
the values an enum argument accepts. Nothing reads it at run time; it is what
you write a query against, so the root name and the paging arguments come from
the pulled schema instead of from memory.

### SDL, and writing the query

A Shopify pull also writes `schema/shopify/shopify.graphql`: the whole schema as
SDL (3552 types for this store, ~3.5 MB). That is what makes the query a
**first-class file** rather than a string in `source.py`:

```
integrations/<name>/queries/*.graphql     the documents, committed
integrations/<name>/schema/shopify/shopify.graphql
graphql.config.yml                        points an editor at both
```

With the [GraphQL extension](https://marketplace.visualstudio.com/items?itemName=GraphQL.vscode-graphql)
installed, completion, hover and errors come from the pulled schema, and
go-to-definition on a field lands in the SDL. Nothing about that is specific to
Otter: any tool that reads SDL works, because the file is the schema.

The same artifact is what CI validates, so a renamed Shopify field fails a test
rather than producing an empty value in a run that still reports `succeeded`:

```sh
pip install -e 'lib/python[dev]'   # graphql-core
python3 -m unittest discover -s integrations/<name>/tests
```

`make sync-schema SYSTEM=shopify` passes `--no-sdl` to skip the SDL when you only
want the Python module. `--depth` bounds the module, not the SDL — the SDL is
always the complete schema, because a truncated schema would make an editor
report false errors.

## What is not here yet

A `--check` that re-describes and diffs the committed schema, to run in CI or
before a deploy; until then a pull is a snapshot with a `Fetched:` timestamp, and
the SDL goes stale the same way the module does.

Drift between what a document *selects* and what the mapping *reads* is only
partly covered. `graphql-core` proves every selected field exists; it cannot
prove the mapping reads a field the document happens to omit, because that
resolves to `""` silently. An integration asserts that itself, over the parsed
document — see `integrations/shopify-product-to-salesforce-product/tests/test_source.py`.
A callable in a mapping is opaque to that check, which is why the one computed
field there names its own reads by hand.
