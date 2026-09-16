"""Pull a Salesforce object's schema and write a Python module for it.

The short form, from the repository root::

    make sync-schema INTEGRATION=shopify-product-to-salesforce-product

Everything else is discovered: the integration directory supplies the object
name, instance URL and API version from its ``otter.yaml``, and the credentials
come from ``otter.env`` at the checkout root. Flags override any of it, and an
explicit environment variable overrides both -- so ``SALESFORCE_OBJECT=Contact
make sync-schema`` retargets without editing a file.

Run directly if you prefer::

    python3 lib/python/otter_schema/pull.py \
        --integration integrations/shopify-product-to-salesforce-product

Output lands in ``<integration>/schema/salesforce/`` and is meant to be
committed. Regenerating is a deliberate act, not something a run does.
"""

import argparse
import ast
import datetime
import os
import sys

# Runnable as a plain script as well as with -m, so neither the Makefile nor a
# developer has to get PYTHONPATH right. This has to happen before the
# otter_connectors import below.
if __package__ in (None, ""):
    sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from otter_connectors.salesforce import DEFAULT_API_VERSION, SalesforceClient  # noqa: E402

from otter_schema.generate import module_name, render_init, render_module  # noqa: E402

__all__ = ["find_repo_root", "main", "object_names", "read_env_file",
           "read_manifest_env"]

#: Where the shared credentials file lives inside a checkout.
SHARED_ENV_FILE = "otter.env"


def find_repo_root(start):
    """Walk up from an integration directory to the checkout root.

    Identified by a ``go.mod`` or a shared ``otter.env``. Returns ``None`` when
    neither is found, which is not an error: every value can come from a flag.
    """
    current = os.path.abspath(start)
    while True:
        if (os.path.exists(os.path.join(current, "go.mod"))
                or os.path.exists(os.path.join(current, SHARED_ENV_FILE))):
            return current
        parent = os.path.dirname(current)
        if parent == current:
            return None
        current = parent


def read_env_file(path):
    """``KEY=value`` pairs: the subset systemd's ``EnvironmentFile=`` accepts.

    A missing file is not an error -- the values may all be in the process
    environment.
    """
    values = {}
    try:
        with open(path) as handle:
            lines = handle.read().splitlines()
    except FileNotFoundError:
        return values

    for number, raw in enumerate(lines, start=1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        line = line.removeprefix("export ").strip()
        key, separator, value = line.partition("=")
        if not separator or not key.strip():
            raise ValueError("%s:%d: expected KEY=value, got %r" % (path, number, raw))
        values[key.strip()] = _strip_quotes(value.strip())
    return values


def read_manifest_env(path):
    """The ``env:`` block of an ``otter.yaml``, as a flat dict.

    Deliberately *not* a YAML parser. It reads the one block this tool needs --
    scalar ``KEY: value`` pairs under a top-level ``env:`` -- and raises on
    anything else rather than guessing, because a silently misread
    ``SALESFORCE_INSTANCE_URL`` would produce a schema for the wrong org. Pass
    the values as flags instead if that happens.

    A missing file is not an error: the manifest is optional.
    """
    try:
        with open(path) as handle:
            lines = handle.read().splitlines()
    except FileNotFoundError:
        return {}

    env = {}
    inside = False
    for number, raw in enumerate(lines, start=1):
        stripped = raw.strip()
        if not stripped or stripped.startswith("#"):
            continue

        if not inside:
            if stripped == "env:":
                inside = True
            continue

        # A non-indented line ends the block.
        if raw[:1] not in (" ", "\t"):
            break

        key, separator, value = stripped.partition(":")
        if not separator or not key.strip():
            raise ValueError("%s:%d: not a KEY: value pair: %r" % (path, number, stripped))
        env[key.strip()] = _unquote(value.strip(), path, number)
    return env


def _strip_quotes(value):
    if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
        return value[1:-1]
    return value


def _unquote(value, path, number):
    value = _strip_quotes(value)
    if " #" in value:
        # A trailing comment on an unquoted scalar. Quote the value in the
        # manifest if it genuinely contains " #".
        value = value.split(" #", 1)[0].strip()
    if not value:
        raise ValueError("%s:%d: empty value" % (path, number))
    return value


def parse_args(argv):
    parser = argparse.ArgumentParser(
        prog="otter_schema.pull",
        description="Write a Salesforce object's schema as a Python module.")
    parser.add_argument("--integration", default=".",
                        help="integration directory (default: the current directory)")
    parser.add_argument("--object", action="append", default=[],
                        help="sObject API name; repeatable and/or comma-separated "
                             "(default: the manifest's SALESFORCE_OBJECT)")
    parser.add_argument("--env-file", default="",
                        help="shared credentials file (default: <checkout>/" + SHARED_ENV_FILE + ")")
    parser.add_argument("--out", default="",
                        help="output directory (default: <integration>/schema/salesforce)")
    parser.add_argument("--instance-url", default="",
                        help="Salesforce My Domain URL (default: the manifest)")
    parser.add_argument("--api-version", default="",
                        help="Salesforce API version (default: the manifest, else "
                             + DEFAULT_API_VERSION + ")")
    parser.add_argument("--picklists", action="store_true",
                        help="embed each picklist's active values")
    parser.add_argument("--dry-run", action="store_true",
                        help="print the module and write nothing")
    return parser.parse_args(argv)


def object_names(values):
    """Expand ``--object`` values into a unique, ordered list.

    Accepts both spellings -- ``--object Contact --object Order`` and
    ``--object Contact,Order`` -- because the Makefile cannot pass a list
    without either repeating the flag or depending on how a shell splits one.
    API names never contain a comma or a space, so splitting is unambiguous.
    """
    names = []
    for value in values:
        for part in value.replace(",", " ").split():
            if part not in names:
                names.append(part)
    return names


def main(argv=None):
    args = parse_args(argv)
    integration = os.path.abspath(args.integration)

    repo = find_repo_root(integration) or os.getcwd()
    env_file = args.env_file or os.path.join(repo, SHARED_ENV_FILE)
    file_env = read_env_file(env_file)
    manifest = read_manifest_env(os.path.join(integration, "otter.yaml"))

    def setting(key, default=""):
        """Flags, then an explicit environment variable, then the env file, then
        the manifest. The order means `KEY=x make sync-schema` overrides without
        editing anything, which is what makes a one-off retarget cheap."""
        return (os.environ.get(key) or file_env.get(key)
                or manifest.get(key) or default)

    objects = object_names(args.object) or [setting("SALESFORCE_OBJECT", "Contact")]
    instance_url = args.instance_url or setting("SALESFORCE_INSTANCE_URL")
    if not instance_url:
        raise SystemExit(
            "otter: no instance URL. Add SALESFORCE_INSTANCE_URL to the manifest's "
            "env: block, or to %s, or pass --instance-url." % env_file)
    api_version = args.api_version or setting("SALESFORCE_API_VERSION", DEFAULT_API_VERSION)
    out_dir = args.out or os.path.join(integration, "schema", "salesforce")

    client = SalesforceClient(
        instance_url=instance_url,
        api_version=api_version,
        auth=setting("SALESFORCE_AUTH", "client_credentials"),
        client_id=setting("SALESFORCE_CLIENT_ID"),
        client_secret=setting("SALESFORCE_CLIENT_SECRET"),
        username=setting("SALESFORCE_USERNAME"),
        password=setting("SALESFORCE_PASSWORD"),
    )

    fetched_at = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

    rendered = []
    fields = 0
    for object_name in objects:
        describe = client.describe(object_name)
        fields += len(describe.get("fields") or [])
        source = render_module(
            object_name, describe,
            instance_url=client.instance_url,
            api_version=api_version,
            fetched_at=fetched_at,
            include_picklists=args.picklists,
        )
        rendered.append((object_name, source))
        if args.dry_run:
            sys.stdout.write(source)

    if args.dry_run:
        return 0

    for object_name, source in rendered:
        _write(os.path.join(out_dir, module_name(object_name) + ".py"), source)

    # Rebuilt from the directory, so pulling one object never drops another.
    names = existing_objects(out_dir) | {name for name, _ in rendered}
    _write(os.path.join(out_dir, "__init__.py"), render_init(names))

    print("pulled %d field(s) for %s into %s"
          % (fields, ", ".join(objects), out_dir))
    return 0


def existing_objects(out_dir):
    """Object names already generated in ``out_dir``, so an index rebuild keeps
    objects that this run did not touch."""
    names = set()
    if not os.path.isdir(out_dir):
        return names
    for entry in sorted(os.listdir(out_dir)):
        if entry == "__init__.py" or not entry.endswith(".py"):
            continue
        try:
            with open(os.path.join(out_dir, entry)) as handle:
                tree = ast.parse(handle.read())
        except (OSError, SyntaxError):
            continue
        for node in tree.body:
            if isinstance(node, ast.ClassDef):
                names.add(node.name)
    return names


def _write(path, content):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as handle:
        handle.write(content)


if __name__ == "__main__":
    sys.exit(main())
