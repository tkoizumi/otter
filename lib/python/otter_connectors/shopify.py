"""A small Shopify Admin API (GraphQL) client.

Covers the three things every Shopify integration needs, and nothing else:

* **Authentication.** Shopify retired admin-created custom apps, so there is no
  permanent token to configure. A server-side app acting on stores in its own
  organisation uses the client credentials grant: it exchanges its own client ID
  and secret for a 24 hour token and renews it by repeating the request. Pass
  ``client_id``/``client_secret``; pass ``token`` only if you still hold a
  pre-generated one.
* **Throttle handling.** Shopify bills GraphQL by query cost from a leaky
  bucket, so a throttle is normal backpressure rather than an error. This waits
  out the bucket instead of failing the run.
* **Cursor paging** over any Relay-style connection.

The Admin REST API is a legacy API, hence GraphQL throughout.
"""

import json
import re
import time
import urllib.error
import urllib.parse
import urllib.request

from .errors import ConfigError, ConnectorError
from .http import http_open

__all__ = ["DEFAULT_API_VERSION", "ShopifyClient", "ShopifyError", "numeric_id"]

#: Pin deliberately and bump on your own schedule; Shopify retires versions.
DEFAULT_API_VERSION = "2026-07"
DEFAULT_TIMEOUT = 60


class ShopifyError(ConnectorError):
    """Shopify could not serve the request."""


class ShopifyClient:
    """Talks to one store's GraphQL Admin API."""

    def __init__(self, store, api_version=DEFAULT_API_VERSION, token=None,
                 client_id=None, client_secret=None, api_base=None, token_url=None,
                 max_attempts=5, timeout=DEFAULT_TIMEOUT):
        self.store = store
        self.api_version = api_version
        self.base = (api_base or "https://%s/admin/api/%s" % (store, api_version)).rstrip("/")
        self.token_url = token_url or "https://%s/admin/oauth/access_token" % store
        self.max_attempts = max_attempts
        self.timeout = timeout

        self._static_token = token
        self.client_id = client_id
        self.client_secret = client_secret
        self._token = None
        self._token_expires_at = 0.0
        self._refreshed = False

    # -- authentication ---------------------------------------------------- #

    def access_token(self, force=False):
        """Return a valid Admin API access token, minting one if needed.

        Otter runs each attempt in a fresh process, so a token is requested once
        per run: nothing to persist and no stale-token failure mode.
        """
        if self._static_token:
            return self._static_token
        if not force and self._token and time.monotonic() < self._token_expires_at:
            return self._token
        if not self.client_id or not self.client_secret:
            raise ConfigError(
                "Shopify authentication is not configured: set SHOPIFY_CLIENT_ID and "
                "SHOPIFY_CLIENT_SECRET (client credentials grant), or SHOPIFY_ACCESS_TOKEN "
                "if you hold a pre-generated token"
            )

        request = urllib.request.Request(
            self.token_url,
            data=urllib.parse.urlencode({
                "grant_type": "client_credentials",
                "client_id": self.client_id,
                "client_secret": self.client_secret,
            }).encode("utf-8"),
            method="POST",
            # Deliberately no `Accept: application/json`: Shopify renders this
            # endpoint's errors as an HTML page, and asking for JSON makes some
            # failures come back with an empty body, which hides the reason.
            headers={"Content-Type": "application/x-www-form-urlencoded"},
        )
        try:
            with http_open(request, self.timeout) as response:
                payload = json.loads(response.read())
        except urllib.error.HTTPError as exc:
            raise ConfigError(token_error(exc))
        except urllib.error.URLError as exc:
            raise ShopifyError("cannot reach the Shopify token endpoint: %s" % exc.reason)

        token = payload.get("access_token")
        if not token:
            raise ConfigError("Shopify token response contained no access_token: %s" % payload)

        # Read expires_in rather than assuming 24 hours, and renew a minute
        # early so a token cannot lapse between two pages of the same run.
        expires_in = float(payload.get("expires_in") or 86399)
        self._token = token
        self._token_expires_at = time.monotonic() + max(expires_in - 60.0, 0.0)
        return token

    # -- GraphQL ----------------------------------------------------------- #

    def graphql(self, query, variables=None):
        """Run a GraphQL document and return its ``data`` object."""
        body = json.dumps({"query": query, "variables": variables or {}}).encode("utf-8")

        for attempt in range(1, self.max_attempts + 1):
            request = urllib.request.Request(
                self.base + "/graphql.json",
                data=body,
                method="POST",
                headers={
                    "Content-Type": "application/json",
                    "Accept": "application/json",
                    "X-Shopify-Access-Token": self.access_token(),
                },
            )
            try:
                with http_open(request, self.timeout) as response:
                    status = response.status
                    headers = response.headers
                    payload = response.read()
            except urllib.error.HTTPError as exc:
                status = exc.code
                headers = exc.headers
                payload = exc.read()
            except urllib.error.URLError as exc:
                if attempt == self.max_attempts:
                    raise ShopifyError("cannot reach Shopify: %s" % exc.reason)
                time.sleep(min(2 ** attempt, 15))
                continue

            if status == 401:
                # The token we hold is not usable: drop it and let the next
                # attempt mint a fresh one. The attempt loop bounds this, so a
                # genuinely bad app cannot spin forever.
                if self._static_token:
                    raise ConfigError(
                        "Shopify rejected the pre-generated SHOPIFY_ACCESS_TOKEN "
                        "(HTTP 401): %s" % payload[:400].decode("utf-8", "replace")
                    )
                self._token = None
                self._token_expires_at = 0.0
                continue

            if status == 403:
                # 403 is usually authorisation rather than authentication -- a
                # missing scope, or a rejected non-expiring token. Try one
                # refresh for the latter, then report what Shopify said.
                if not self._static_token and not self._refreshed:
                    self._refreshed = True
                    self._token = None
                    self._token_expires_at = 0.0
                    continue
                raise ConfigError(
                    "Shopify refused the request (HTTP 403): %s. Check the "
                    "read_customers scope on the app version, and that the app is "
                    "approved for protected customer data."
                    % payload[:400].decode("utf-8", "replace")
                )

            if status == 429:
                time.sleep(min(float(headers.get("Retry-After") or 2), 30))
                continue
            if status >= 500:
                if attempt == self.max_attempts:
                    raise ShopifyError(
                        "Shopify returned HTTP %d after %d attempts" % (status, attempt))
                time.sleep(min(2 ** attempt, 15))
                continue
            if status != 200:
                raise ShopifyError("Shopify returned HTTP %d: %s" % (status, payload[:400]))

            try:
                document = json.loads(payload)
            except ValueError:
                raise ShopifyError("Shopify returned a non-JSON response: %s" % payload[:400])

            errors = document.get("errors") or []
            if errors:
                if all(is_throttled(error) for error in errors):
                    time.sleep(throttle_delay(document))
                    continue
                raise ShopifyError("Shopify GraphQL error: %s" % json.dumps(errors)[:600])

            return document.get("data") or {}

        raise ShopifyError("Shopify request failed after %d attempts" % self.max_attempts)

    def connection(self, query, variables=None, path="customers"):
        """One page of a Relay connection, as ``(nodes, page_info)``.

        Drive the loop yourself when the cursor needs checkpointing::

            nodes, info = client.connection(QUERY, {"first": 100, "after": cursor}, "customers")
            cursor = info.get("endCursor")
        """
        connection = self.graphql(query, variables).get(path) or {}
        return connection.get("nodes") or [], connection.get("pageInfo") or {}


# --------------------------------------------------------------------------- #
# Helpers
# --------------------------------------------------------------------------- #


def numeric_id(gid):
    """The trailing id of a Shopify GID.

    ``gid://shopify/Customer/1234`` -> ``"1234"``. Useful as an external ID on
    the other side of a sync, where the opaque GID is not welcome; a value that
    is already just an id is returned unchanged.
    """
    return str(gid or "").rsplit("/", 1)[-1]


# --------------------------------------------------------------------------- #
# Error rendering
# --------------------------------------------------------------------------- #


def is_throttled(error):
    """Whether a GraphQL error is Shopify asking us to slow down."""
    extensions = (error or {}).get("extensions") or {}
    return extensions.get("code") == "THROTTLED"


def throttle_delay(document):
    """How long to wait for the leaky bucket to afford the next query."""
    cost = ((document.get("extensions") or {}).get("cost")) or {}
    status = cost.get("throttleStatus") or {}
    needed = cost.get("requestedQueryCost") or 100
    available = status.get("currentlyAvailable") or 0
    restore_rate = status.get("restoreRate") or 50
    return min(max(1.0, (needed - available) / max(restore_rate, 1)), 30.0)


def token_error(exc):
    """Turn a token endpoint failure into something actionable.

    The endpoint answers with an HTML error page by default and JSON when asked,
    but some failures return no body at all under JSON negotiation, which is how
    a real problem ends up looking like a blank message.
    """
    text = exc.read().decode("utf-8", "replace").strip()
    detail = error_detail(text) or "no response body"

    if "shop_not_permitted" in text:
        return (
            "Shopify refused the client credentials grant (shop_not_permitted). The app and "
            "the store must belong to the same Shopify organization, and the store must be "
            "listed under Dev stores in the Dev Dashboard; having the app installed is not "
            "enough. If the store belongs to someone else (a client), client credentials "
            "cannot reach it -- distribute the app with custom distribution and use the "
            "authorization code grant instead."
        )
    if exc.code in (400, 401, 403):
        return "Shopify rejected the app credentials (HTTP %d): %s" % (exc.code, detail)
    return "Shopify token request failed (HTTP %d): %s" % (exc.code, detail)


def error_detail(text):
    """Extract a readable message from a JSON or HTML error response."""
    if not text:
        return ""
    try:
        payload = json.loads(text)
    except ValueError:
        payload = None
    if isinstance(payload, dict):
        for key in ("error_description", "error", "message"):
            if payload.get(key):
                return str(payload[key])[:300]

    match = re.search(r"<title[^>]*>(.*?)</title>", text, re.S | re.I)
    if match:
        return re.sub(r"\s+", " ", match.group(1)).strip()[:300]
    match = re.search(r'"(?:error_description|error|message)"\s*:\s*"([^"]+)"', text)
    if match:
        return match.group(1)[:300]
    return re.sub(r"\s+", " ", text)[:300]
