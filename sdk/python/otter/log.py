"""Structured logging for Otter integrations.

Every call sends one synchronous JSON record to the daemon:

    POST /v1/runs/{run_id}/logs
    {"stream": "otter", "message": "...", "fields": {"level": "info", ...}}

The daemon's log endpoint accepts only ``stream``/``message``/``fields``, so the
severity is carried inside ``fields`` under the ``level`` key.

Logging is best effort by design: if the request keeps failing, the record is
written to stderr as a single JSON line and the integration continues. A logging
failure must never crash an integration.
"""

import json
import sys
import threading
from typing import Any, Optional, TextIO

from ._client import Client, encode_path_segment

__all__ = ["Logger"]

#: Marks a line as having come from the integration's logger rather than from the
#: runtime's own narration, which shares the same log stream.
_LOGGER_NAME = "otter"


class Logger:
    """Thread-safe structured logger bound to a single run."""

    def __init__(
        self,
        client: Client,
        run_id: str,
        fallback_stream: Optional[TextIO] = None,
    ) -> None:
        self._client = client
        self._run_id = run_id
        self._fallback_stream = fallback_stream
        self._lock = threading.Lock()

    # -- levels ---------------------------------------------------------

    def debug(self, message: Any, **fields: Any) -> None:
        """Log at ``debug`` level."""
        self._log("debug", message, fields)

    def info(self, message: Any, **fields: Any) -> None:
        """Log at ``info`` level."""
        self._log("info", message, fields)

    def warning(self, message: Any, **fields: Any) -> None:
        """Log at ``warning`` level."""
        self._log("warning", message, fields)

    def warn(self, message: Any, **fields: Any) -> None:
        """Alias for :meth:`warning`."""
        self._log("warning", message, fields)

    def error(self, message: Any, **fields: Any) -> None:
        """Log at ``error`` level."""
        self._log("error", message, fields)

    # -- internals ------------------------------------------------------

    def _log(self, level: str, message: Any, fields: dict) -> None:
        text = self._as_text(message)
        payload_fields = dict(fields)
        payload_fields["level"] = level
        # The stream alone does not identify the writer: the daemon narrates a
        # run's lifecycle on the same stream. This marker states that the line
        # came from the integration's own logger, so a reader never has to guess
        # from the payload's shape.
        payload_fields["logger"] = _LOGGER_NAME
        path = "/v1/runs/%s/logs" % encode_path_segment(self._run_id)
        with self._lock:
            try:
                status, _body = self._client.post_json(
                    path, {"stream": "otter", "message": text, "fields": payload_fields}
                )
                if 200 <= status < 300:
                    return
            except Exception:  # never let logging break the integration
                pass
            self._write_fallback(level, text, fields)

    @staticmethod
    def _as_text(message: Any) -> str:
        if isinstance(message, str):
            return message
        try:
            return str(message)
        except Exception:
            return repr(message)

    def _write_fallback(self, level: str, message: str, fields: dict) -> None:
        try:
            stream = self._fallback_stream if self._fallback_stream is not None else sys.stderr
            line = json.dumps(
                {"level": level, "message": message, "fields": fields},
                default=str,
            )
            stream.write(line + "\n")
            stream.flush()
        except Exception:
            pass
