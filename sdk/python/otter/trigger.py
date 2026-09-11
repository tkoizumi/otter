"""Trigger information for the current Otter run.

The trigger type is normally available from the environment
(``OTTER_TRIGGER_TYPE``). The webhook payload (``body`` and ``headers``) lives on
the run object, so it is fetched lazily from ``GET /v1/runs/{run_id}`` on first
access and cached for the rest of the run.
"""

from typing import Any, Dict, List, Optional

from ._client import Client, OtterError, encode_path_segment

__all__ = ["Trigger"]

_KNOWN_TYPES = ("manual", "cron", "webhook")


class Trigger:
    """Read-only view of how the current run was started."""

    def __init__(
        self,
        client: Client,
        run_id: str,
        trigger_type: Optional[str] = None,
    ) -> None:
        self._client = client
        self._run_id = run_id
        self._trigger_type = trigger_type
        self._metadata: Optional[Dict[str, Any]] = None
        self._loaded = False

    # -- public API -----------------------------------------------------

    @property
    def type(self) -> str:
        """``manual``, ``cron``, ``webhook`` or ``unknown``.

        The environment value wins; the run metadata's ``type`` is the fallback.
        """
        if isinstance(self._trigger_type, str):
            candidate = self._trigger_type.strip().lower()
            if candidate in _KNOWN_TYPES:
                return candidate
        meta_type = self.metadata.get("type")
        if isinstance(meta_type, str) and meta_type.strip().lower() in _KNOWN_TYPES:
            return meta_type.strip().lower()
        return "unknown"

    @property
    def metadata(self) -> Dict[str, Any]:
        """The run's full ``metadata`` object (``{}`` when there is none)."""
        return dict(self._load_metadata())

    @property
    def body(self) -> Any:
        """Parsed webhook body, or ``None`` for manual/cron runs."""
        return self._load_metadata().get("body")

    @property
    def headers(self) -> Dict[str, List[str]]:
        """Webhook headers as ``{name: [values]}``, or ``{}``."""
        headers = self._load_metadata().get("headers")
        if isinstance(headers, dict):
            return headers
        return {}

    def header(self, name: str) -> Optional[Any]:
        """Return the first value of header ``name`` (case-insensitive), or ``None``."""
        if not isinstance(name, str):
            return None
        wanted = name.strip().lower()
        for header_name, value in self.headers.items():
            if str(header_name).strip().lower() != wanted:
                continue
            if isinstance(value, (list, tuple)):
                return value[0] if value else None
            return value
        return None

    # -- internals ------------------------------------------------------

    def _load_metadata(self) -> Dict[str, Any]:
        """Fetch and cache the run's metadata; failures degrade to ``{}``."""
        if self._loaded:
            return self._metadata if self._metadata is not None else {}
        self._loaded = True
        self._metadata = {}
        try:
            status, body = self._client.get_json(
                "/v1/runs/%s" % encode_path_segment(self._run_id)
            )
        except OtterError:
            return self._metadata
        if status == 200 and isinstance(body, dict):
            metadata = body.get("metadata")
            if isinstance(metadata, dict):
                self._metadata = metadata
        return self._metadata
