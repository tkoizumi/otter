"""Internal HTTP client for the Otter Python SDK.

This module is deliberately small and has no third-party dependencies. It speaks
the JSON HTTP API exposed by the ``otterd`` daemon:

* every request carries ``Authorization: Bearer <OTTER_STATE_TOKEN>`` when a
  token is configured,
* request bodies are JSON with ``Content-Type: application/json``,
* transient failures (connection errors and HTTP 5xx) are retried with a short
  exponential backoff, while 4xx responses are returned to the caller as-is.

Integrations normally never touch this module directly: they use
:class:`otter.Context`, whose ``state``, ``log`` and ``trigger`` helpers wrap it.
"""

import http.client
import json
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Optional, Tuple

from . import _capture

__all__ = ["Client", "OtterError", "encode_path_segment"]


class OtterError(Exception):
    """Raised when the SDK cannot complete an operation.

    Typical causes: a missing ``OTTER_API_URL``, a non JSON-serializable state
    value, an invalid state key, or an API request that failed on transport
    level after all retries were exhausted.
    """


def encode_path_segment(value: Any) -> str:
    """Percent-encode ``value`` so it can be used as a single URL path segment."""
    return urllib.parse.quote(str(value), safe="")


def _decode_body(raw: Optional[bytes]) -> Any:
    """Decode a JSON HTTP body, returning ``None`` when it is empty or invalid."""
    if not raw:
        return None
    try:
        return json.loads(raw.decode("utf-8"))
    except (ValueError, UnicodeDecodeError):
        return None


class Client:
    """Minimal JSON HTTP client for the Otter daemon API.

    Args:
        api_url: Base URL of the daemon, e.g. ``http://127.0.0.1:7337``.
        token: Optional ``OTTER_STATE_TOKEN`` bearer token.
        timeout: Per-request timeout in seconds.

    The public helpers return ``(status_code, parsed_json_or_None)``. HTTP error
    responses (including 4xx) are returned rather than raised, because callers
    such as :class:`otter.State` need to distinguish "not found" from failure.
    Only transport-level failures that survive every retry raise
    :class:`OtterError`.
    """

    #: Total number of attempts made for a single request (1 try + 2 retries).
    MAX_ATTEMPTS = 3
    #: Delay before retry N; only the first ``MAX_ATTEMPTS - 1`` entries apply.
    RETRY_DELAYS = (0.1, 0.2, 0.4)

    def __init__(self, api_url: str, token: Optional[str] = None, timeout: float = 10.0) -> None:
        if not api_url or not str(api_url).strip():
            raise OtterError(
                "an Otter API URL is required (set OTTER_API_URL, "
                "for example http://127.0.0.1:7337)"
            )
        self.api_url = str(api_url).strip().rstrip("/")
        self.token = token or None
        self.timeout = timeout
        self.max_attempts = self.MAX_ATTEMPTS
        self.retry_delays = self.RETRY_DELAYS

    # -- public helpers -------------------------------------------------

    def get_json(self, path: str) -> Tuple[int, Any]:
        """``GET path``; returns ``(status, parsed_body)``."""
        return self.request("GET", path)

    def put_json(self, path: str, value: Any) -> Tuple[int, Any]:
        """``PUT path`` with ``value`` serialized as the JSON body."""
        return self.request("PUT", path, encode_json(value))

    def post_json(self, path: str, payload: Any) -> Tuple[int, Any]:
        """``POST path`` with ``payload`` serialized as the JSON body."""
        return self.request("POST", path, encode_json(payload))

    def delete_json(self, path: str) -> Tuple[int, Any]:
        """``DELETE path``; returns ``(status, parsed_body)``."""
        return self.request("DELETE", path)

    # -- internals ------------------------------------------------------

    def request(self, method: str, path: str, body: Optional[bytes] = None) -> Tuple[int, Any]:
        """Perform one HTTP request, retrying transient failures."""
        url = self.api_url + path
        attempt = 0
        while True:
            attempt += 1
            request = urllib.request.Request(url, data=body, method=method)
            request.add_header("Accept", "application/json")
            if body is not None:
                request.add_header("Content-Type", "application/json")
            if self.token:
                request.add_header("Authorization", "Bearer %s" % self.token)
            try:
                # Otter's own control traffic is never captured: recording the
                # recorder would recurse, and the daemon API is not integration
                # behaviour. The guard is thread-local, so it cannot hide a
                # concurrent request the integration itself makes.
                with _capture.suppressed():
                    with urllib.request.urlopen(request, timeout=self.timeout) as response:
                        return int(response.status), _decode_body(response.read())
            except urllib.error.HTTPError as exc:
                payload = _decode_body(exc.read())
                if 500 <= int(exc.code) < 600 and attempt < self.max_attempts:
                    self._backoff(attempt)
                    continue
                return int(exc.code), payload
            except (urllib.error.URLError, http.client.HTTPException, OSError) as exc:
                if attempt < self.max_attempts:
                    self._backoff(attempt)
                    continue
                raise OtterError(
                    "%s %s failed after %d attempt(s): %s" % (method, url, attempt, exc)
                )

    def _backoff(self, attempt: int) -> None:
        delays = self.retry_delays or ()
        if not delays:
            return
        delay = delays[min(attempt - 1, len(delays) - 1)]
        if delay > 0:
            time.sleep(delay)


def encode_json(value: Any) -> bytes:
    """Serialize ``value`` to compact UTF-8 JSON bytes.

    Raises :class:`OtterError` when the value is not JSON-serializable
    (``NaN``/``Infinity`` are rejected too, since they are not valid JSON).
    """
    try:
        return json.dumps(value, allow_nan=False, separators=(",", ":")).encode("utf-8")
    except (TypeError, ValueError) as exc:
        raise OtterError("value is not JSON-serializable: %s" % (exc,))
