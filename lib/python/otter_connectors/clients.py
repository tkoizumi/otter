"""Building the shared clients from the standard environment settings.

``shopify.py`` and ``salesforce.py`` are vendor clients: they take plain
arguments and know nothing about how an integration is configured. This module
is the one place that maps the ``SHOPIFY_*`` and ``SALESFORCE_*`` settings onto
them, so an integration does not restate seven constructor arguments to build a
client -- and so the several call sites cannot drift apart.

The readers take a ``get`` callable rather than reaching for ``os.environ``
themselves, because not every caller resolves settings the same way.
``otter_schema/pull.py`` reads CLI flags, then an environment variable, then a
shared env file, then the manifest, and passes that lookup in.

Two conventions here are deliberate:

* **The store and the instance URL are arguments, not settings.** A
  ``ShopifyClient`` bakes its store into its API and token URLs and caches one
  store's token, so it is never store-agnostic; a ``SalesforceClient`` is bound
  to one org. Keeping them explicit is what stops a future multi-store or
  multi-org run from silently reusing whichever target the environment happened
  to name.
* **Credentials are read, not passed.** One Shopify app -- and one Salesforce
  connected app -- can act on every store or org it is installed on, so the
  client id and secret are per-deployment rather than per-target.

An ``overrides`` keyword forwards anything the caller computed itself, such as
an API version from a command line flag.
"""

from .config import env
from .errors import ConfigError
from .salesforce import DEFAULT_API_VERSION as SALESFORCE_API_VERSION
from .salesforce import SalesforceClient
from .shopify import DEFAULT_API_VERSION as SHOPIFY_API_VERSION
from .shopify import ShopifyClient

__all__ = ["salesforce_client", "shopify_client"]


def shopify_client(store, get=env, **overrides):
    """A ``ShopifyClient`` for ``store``, from the standard ``SHOPIFY_*`` settings.

    ``store`` is required and has no environment fallback: the client is only
    ever bound to one store, and a default here would let one store's setting
    drive every store in a run that syncs more than one.
    """
    settings = {
        "api_version": get("SHOPIFY_API_VERSION", SHOPIFY_API_VERSION),
        # Only set for a legacy pre-generated token; otherwise the client
        # credentials grant supplies a fresh 24 hour token per run.
        "token": get("SHOPIFY_ACCESS_TOKEN"),
        "client_id": get("SHOPIFY_CLIENT_ID"),
        "client_secret": get("SHOPIFY_CLIENT_SECRET"),
        "api_base": get("SHOPIFY_API_BASE"),
        "token_url": get("SHOPIFY_TOKEN_URL"),
    }
    settings.update(overrides)
    settings["store"] = store
    return ShopifyClient(**settings)


def salesforce_client(instance_url, get=env, **overrides):
    """A ``SalesforceClient`` for ``instance_url``, from the ``SALESFORCE_*`` settings.

    ``instance_url`` is required for the same reason ``store`` is on the Shopify
    side: a client talks to exactly one org.
    """
    settings = {
        "api_version": get("SALESFORCE_API_VERSION", SALESFORCE_API_VERSION),
        "auth": get("SALESFORCE_AUTH", "client_credentials"),
        "client_id": get("SALESFORCE_CLIENT_ID"),
        "client_secret": get("SALESFORCE_CLIENT_SECRET"),
        "username": get("SALESFORCE_USERNAME"),
        "password": get("SALESFORCE_PASSWORD"),
        "batch_size": _int(get, "SALESFORCE_BATCH_SIZE", 200),
    }
    settings.update(overrides)
    settings["instance_url"] = instance_url
    return SalesforceClient(**settings)


def _int(get, name, default):
    """Read an integer setting through ``get``.

    ``config.env_int`` reads the environment directly, so it cannot serve a
    caller whose settings come from flags, a file and a manifest. This is the
    same behaviour, and the same error, against any reader.
    """
    raw = get(name)
    if raw is None or str(raw).strip() == "":
        return default
    try:
        return int(str(raw).strip())
    except ValueError:
        raise ConfigError("%s must be an integer, got %r" % (name, raw))
