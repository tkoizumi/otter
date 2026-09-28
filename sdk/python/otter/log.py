"""Structured logging for Otter integrations.

Every call sends one synchronous JSON record to the daemon:

    POST /v1/runs/{run_id}/logs
    {"stream": "otter", "message": "...", "fields": {"level": "info", ...}}

The daemon's log endpoint accepts only ``stream``/``message``/``fields``, so the
severity is carried inside ``fields`` under the ``level`` key.

Logging is best effort by design: field values JSON cannot represent are
rendered with ``str()`` rather than dropped. When a record still cannot be
delivered it is written to stderr as a single JSON line carrying a
``delivery_error`` that names the reason, and the integration continues. A
logging failure must never crash an integration -- and must never be invisible
either: a degraded record that looks like an ordinary one is worse than no
record, because it is trusted.
"""

import json
import sys
import threading
from typing import Any, Optional, TextIO

from ._client import Client, describe_api_failure, encode_path_segment

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
            reason = self._deliver(path, text, payload_fields)
            if reason is None:
                return
            self._write_fallback(level, text, payload_fields, reason)

    def _deliver(self, path: str, text: str, payload_fields: dict) -> Optional[str]:
        """Send one record; return ``None`` on success, else why it was not sent.

        Every failure is turned into a reason string rather than an exception,
        because a log call must never break the integration. The reason is not
        discarded: it is what tells the fallback reader that this record was
        degraded, and why.
        """
        try:
            # default=str: a log call must not be lost because a field is, say,
            # a pathlib.Path or a datetime. The daemon stores JSON, so an
            # unrepresentable value is rendered rather than rejected -- unlike
            # state writes, which stay strict on purpose.
            status, body = self._client.post_json(
                path,
                {"stream": "otter", "message": text, "fields": payload_fields},
                default=str,
            )
        except Exception as exc:  # never let logging break the integration
            return "%s: %s" % (type(exc).__name__, exc)
        if 200 <= status < 300:
            return None
        return describe_api_failure(status, body)

    @staticmethod
    def _as_text(message: Any) -> str:
        if isinstance(message, str):
            return message
        try:
            return str(message)
        except Exception:
            return repr(message)

    def _write_fallback(
        self, level: str, message: str, payload_fields: dict, delivery_error: str
    ) -> None:
        # The fallback line is the record that could not be delivered, so it
        # carries the same fields the request would have sent -- including the
        # ``logger`` marker -- plus the reason delivery failed. Without that
        # reason the line reads as an ordinary log with an odd shape, which is
        # exactly how a degraded record goes unnoticed.
        try:
            stream = self._fallback_stream if self._fallback_stream is not None else sys.stderr
            record = {
                "level": level,
                "message": message,
                "fields": payload_fields,
                "delivery_error": delivery_error,
            }
            try:
                line = json.dumps(record, default=str)
            except Exception as exc:
                # The fields themselves defeat json.dumps -- a dict that
                # contains itself, a __str__ that raises. Report the record
                # without them rather than letting the failure vanish: a log
                # call must never leave no trace at all.
                line = json.dumps(
                    {
                        "level": level,
                        "message": message,
                        "delivery_error": delivery_error,
                        "fields_error": "%s: %s" % (type(exc).__name__, exc),
                    }
                )
            stream.write(line + "\n")
            stream.flush()
        except Exception:
            pass
