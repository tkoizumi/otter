"""Integration state, stored by the Otter daemon.

The daemon is the single source of truth for state; this class performs no
caching and holds no local copy. Every call is one HTTP round trip.

    ctx.state.get("count", 0)
    ctx.state.set("count", 1)
    ctx.state.delete("count")
    ctx.state.all()
"""

import re
from typing import Any, Dict, Optional

from ._client import Client, OtterError, encode_path_segment

__all__ = ["State"]

#: Keys are URL path segments; the daemon enforces the same pattern.
_KEY_PATTERN = re.compile(r"^[A-Za-z0-9._:-]{1,128}$")


def _describe_failure(status: int, body: Any) -> str:
    """Extract a human readable message from an API error envelope."""
    if isinstance(body, dict):
        error = body.get("error")
        if isinstance(error, dict) and error.get("message"):
            return "%s (HTTP %s)" % (error["message"], status)
    return "HTTP %s" % (status,)


class State:
    """Key/value state scoped to one integration."""

    def __init__(self, client: Client, integration_id: str) -> None:
        self._client = client
        self._integration_id = integration_id

    # -- public API -----------------------------------------------------

    def get(self, key: str, default: Any = None) -> Any:
        """Return the JSON value stored at ``key``, or ``default`` when unset."""
        status, body = self._client.get_json(self._key_path(key))
        if status == 404:
            return default
        if status >= 400:
            raise OtterError("could not read state key %r: %s" % (key, _describe_failure(status, body)))
        return body

    def set(self, key: str, value: Any) -> Any:
        """Persist ``value`` at ``key`` and return the value the daemon stored.

        Raises :class:`OtterError` for non JSON-serializable values or API
        failures.
        """
        status, body = self._client.put_json(self._key_path(key), value)
        if status >= 400:
            raise OtterError("could not write state key %r: %s" % (key, _describe_failure(status, body)))
        if isinstance(body, dict) and "value" in body:
            return body["value"]
        return value

    def delete(self, key: str) -> bool:
        """Delete ``key``; return ``True`` when it existed, ``False`` otherwise."""
        status, body = self._client.delete_json(self._key_path(key))
        if status == 404:
            return False
        if status >= 400:
            raise OtterError("could not delete state key %r: %s" % (key, _describe_failure(status, body)))
        return True

    def all(self) -> Dict[str, Any]:
        """Return every key/value pair for this integration as a dict."""
        status, body = self._client.get_json(self._base_path())
        if status >= 400:
            raise OtterError("could not list integration state: %s" % _describe_failure(status, body))
        if isinstance(body, dict) and isinstance(body.get("state"), dict):
            return dict(body["state"])
        return {}

    # -- internals ------------------------------------------------------

    def _base_path(self) -> str:
        return "/v1/integrations/%s/state" % encode_path_segment(self._integration_id)

    def _key_path(self, key: str) -> str:
        if not isinstance(key, str) or not _KEY_PATTERN.match(key):
            raise OtterError(
                "invalid state key %r: keys must match [A-Za-z0-9._:-]{1,128}" % (key,)
            )
        return "%s/%s" % (self._base_path(), encode_path_segment(key))
