"""Bounded HTTP capture for Otter integrations.

This module is the SDK half of request inspection. It records what an
integration's own code sends and receives, redacts it, and delivers it to the
daemon in bounded batches.

Three rules shape everything here:

* **Capture never changes behaviour.** A failure to record must not alter a
  return value, an exception or an exit status. Every path that touches the
  network is wrapped so that diagnostic work can only ever be dropped.
* **The recording is bounded.** Bodies, headers, URLs, error text, queue depth,
  batch size and shutdown time all have hard ceilings. Dropping capture is always
  preferable to delaying or growing an integration.
* **Nothing secret is persisted.** Redaction runs here, before delivery, and
  again in the daemon. Neither pass trusts the other.

The transport is not the SDK's normal API client: capture is uploaded with
``http.client`` directly so that recording can never recurse into recording.
"""

import contextlib
import http.client
import json
import os
import re
import signal
import sys
import threading
import time
import uuid
from collections import deque
from datetime import datetime, timezone
from typing import Any, Dict, List, Optional, Tuple
from urllib.parse import unquote_plus, urlsplit, urlunsplit

__all__ = [
    "Capture",
    "Redactor",
    "install_from_environment",
    "flush_before_exit",
    "suppressed",
    "is_suppressed",
    "SCHEMA_VERSION",
    "POLICY_OFF",
    "POLICY_METADATA",
    "POLICY_FULL",
    "REDACTED",
]

#: Wire contract version. It must match inspection.SchemaVersion in the daemon.
SCHEMA_VERSION = 1

POLICY_OFF = "off"
POLICY_METADATA = "metadata"
POLICY_FULL = "full"

#: Value that replaces anything the policy classifies as a credential.
REDACTED = "REDACTED"

# Limits. These mirror inspection.Limits: the SDK keeps a run from producing
# unbounded work, and the daemon independently refuses anything larger.
MAX_BODY_BYTES = 256 * 1024
MAX_HEADER_PAIRS = 100
MAX_HEADER_BYTES = 16 * 1024
MAX_URL_LENGTH = 4096
MAX_CALL_SITE_LENGTH = 512
MAX_BATCH_EVENTS = 100
MAX_ERROR_TEXT_BYTES = 1024
MAX_METHOD_LENGTH = 32

#: Hard ceiling on one serialised batch, kept well under the daemon's 1 MiB
#: request-body limit so that escaping or envelope overhead cannot push it over.
MAX_BATCH_BYTES = 480 * 1024

#: Bounded queue of pending events. Overflow drops the oldest capture.
QUEUE_MAX_EVENTS = 512

#: How long shutdown may spend delivering capture. The daemon sends SIGTERM and
#: escalates to SIGKILL after a grace period, so this must stay comfortably
#: inside it.
FLUSH_DEADLINE_SECONDS = 2.0

#: Per-attempt delivery timeout and attempts. Capture has short timeouts and
#: bounded retries; it must not hold an integration open.
DELIVERY_TIMEOUT_SECONDS = 1.0
DELIVERY_ATTEMPTS = 2

# Body states and omission reasons. They match the daemon's vocabulary exactly.
BODY_EMPTY = "empty"
BODY_CAPTURED = "captured"
BODY_OMITTED = "omitted"

REASON_UNSUPPORTED = "unsupported_content"
REASON_STREAM = "stream_unsupported"
REASON_OVERSIZED = "oversized"
REASON_INCOMPLETE = "incomplete"
REASON_ENCODED = "encoded"
REASON_UNPARSEABLE = "unparseable"
REASON_REDACTION_FAILED = "redaction_failed"
REASON_QUOTA = "quota_exceeded"
REASON_DROPPED = "dropped"
REASON_EXPIRED = "expired"

FINALIZATION_COMPLETE = "complete"
FINALIZATION_INCOMPLETE = "incomplete"

EVENT_STARTED = "request.started"
EVENT_RESPONSE = "request.response"
EVENT_COMPLETED = "request.completed"


# ---------------------------------------------------------------- suppression

_suppress_state = threading.local()


@contextlib.contextmanager
def suppressed():
    """Run a block whose HTTP calls must never be captured.

    The SDK's own daemon traffic uses this. It is thread-local, so suppressing
    one request never hides an integration's concurrent traffic.
    """
    depth = getattr(_suppress_state, "depth", 0)
    _suppress_state.depth = depth + 1
    try:
        yield
    finally:
        _suppress_state.depth = depth


def is_suppressed() -> bool:
    """Report whether the current thread is inside a suppressed block."""
    return getattr(_suppress_state, "depth", 0) > 0


# ------------------------------------------------------------------ redaction

_SENSITIVE_HEADERS = (
    "authorization", "proxyauthorization", "cookie", "setcookie",
    "xapikey", "apikey", "xauthtoken", "xaccesstoken", "xshopifyaccesstoken",
    "xamzsecuritytoken", "xcsrftoken", "csrftoken", "privatetoken",
    "xgitlabtoken", "xhubsignature", "xgoogapikey",
    "ocpapimsubscriptionkey", "subscriptionkey", "xamzcredential", "xamzsignature",
)

_SENSITIVE_QUERY = (
    "accesstoken", "token", "refreshtoken", "idtoken", "apikey", "key",
    "secret", "clientsecret", "password", "passwd", "pwd", "signature", "sig",
    "auth", "authorization", "code", "session", "sessionid", "subscriptionkey",
    "xamzsignature", "xamzcredential", "xamzsecuritytoken",
)

_SENSITIVE_FIELDS = (
    "password", "passwd", "pwd", "secret", "clientsecret", "token",
    "accesstoken", "refreshtoken", "idtoken", "apikey", "privatekey",
    "authorization", "credentials", "cookie", "setcookie", "session",
    "sessionid", "signature", "nonce",
)

_SENSITIVE_MARKERS = (
    "password", "passwd", "pwd", "secret", "token", "apikey", "privatekey",
    "credential", "signature", "authorization", "cookie", "session",
)

_USERINFO_RE = re.compile(r"(?i)([a-z][a-z0-9+.-]*://)[^/@\s]*@")
_TEXT_SECRET_RE = re.compile(
    r"(?i)\b([a-z0-9_.-]*(?:token|secret|password|passwd|pwd|apikey|api_key"
    r"|signature|sig|session|auth)[a-z0-9_.-]*)=([^&\s'\"]+)"
)


def _normalize_name(name: str) -> str:
    return re.sub(r"[\s_-]+", "", str(name).strip().lower())


def _matches(name: str, exact: Tuple[str, ...]) -> bool:
    normalized = _normalize_name(name)
    if normalized in exact:
        return True
    return any(marker in normalized for marker in _SENSITIVE_MARKERS)


def _clean_text(value: Any) -> str:
    """Remove control characters, including CR and LF."""
    text = "" if value is None else str(value)
    return "".join(ch if ch >= " " and ch != "\x7f" else (" " if ch == "\t" else "")
                   for ch in text)


class _RawNumber(str):
    """A JSON number kept as its original literal.

    Parsing numbers as strings and re-emitting them verbatim is what stops
    redaction from rewriting ``1.500`` into ``1.5`` or losing precision on a
    large integer id.
    """


def _dump_json(value: Any) -> str:
    if isinstance(value, _RawNumber):
        return str(value)
    if value is True:
        return "true"
    if value is False:
        return "false"
    if value is None:
        return "null"
    if isinstance(value, str):
        return json.dumps(value, ensure_ascii=False)
    if isinstance(value, (list, tuple)):
        return "[" + ",".join(_dump_json(item) for item in value) + "]"
    if isinstance(value, dict):
        return "{" + ",".join(
            json.dumps(str(key), ensure_ascii=False) + ":" + _dump_json(item)
            for key, item in value.items()
        ) + "}"
    raise TypeError("unsupported JSON value: %r" % (type(value),))


class Redactor:
    """Applies the capture redaction policy.

    The mandatory rules cannot be weakened. ``extra_*`` arguments only add.
    """

    def __init__(
        self,
        extra_headers: Optional[List[str]] = None,
        extra_query: Optional[List[str]] = None,
        extra_fields: Optional[List[str]] = None,
    ) -> None:
        self.headers = tuple(_SENSITIVE_HEADERS) + tuple(
            _normalize_name(n) for n in (extra_headers or ())
        )
        self.query = tuple(_SENSITIVE_QUERY) + tuple(
            _normalize_name(n) for n in (extra_query or ())
        )
        self.fields = tuple(_SENSITIVE_FIELDS) + tuple(
            _normalize_name(n) for n in (extra_fields or ())
        )

    # -- urls -----------------------------------------------------------

    def sanitize_url(self, raw: str) -> Tuple[str, int]:
        """Remove userinfo and the fragment, and redact sensitive query values.

        Repeated parameters and non-sensitive values are preserved.
        """
        if not raw:
            return "", 0
        text = _clean_text(raw)
        try:
            parts = urlsplit(text)
        except ValueError:
            return _strip_unparseable_url(text), 0

        count = 0
        netloc = parts.netloc
        if "@" in netloc:
            netloc = netloc.rsplit("@", 1)[1]
            count += 1

        query = ""
        if parts.query:
            query, redacted = self._sanitize_query(parts.query)
            count += redacted

        sanitized = urlunsplit((parts.scheme, netloc, parts.path, query, ""))
        return _bound(sanitized, MAX_URL_LENGTH), count

    def _sanitize_query(self, raw_query: str) -> Tuple[str, int]:
        parts = raw_query.split("&")
        count = 0
        for index, part in enumerate(parts):
            if not part:
                continue
            name = part.split("=", 1)[0]
            try:
                decoded = unquote_plus(name)
            except Exception:
                decoded = name
            if _matches(decoded, self.query):
                parts[index] = name + "=" + REDACTED
                count += 1
        return "&".join(parts), count

    # -- headers --------------------------------------------------------

    def sanitize_headers(self, pairs: List[Tuple[str, str]]) -> Tuple[List[Dict[str, str]], int]:
        """Redact sensitive header values, preserving order and duplicates."""
        out: List[Dict[str, str]] = []
        count = 0
        total = 0
        for name, value in pairs:
            if len(out) >= MAX_HEADER_PAIRS:
                break
            clean_name = _clean_text(name)
            clean_value = _clean_text(value)
            if _matches(clean_name, self.headers):
                clean_value = REDACTED
                count += 1
            total += len(clean_name) + len(clean_value)
            if total > MAX_HEADER_BYTES:
                break
            out.append({"name": clean_name, "value": clean_value})
        return out, count

    # -- json -----------------------------------------------------------

    def sanitize_json(self, raw: bytes) -> Tuple[bytes, int]:
        """Return a redacted copy of a JSON document.

        Raises :class:`ValueError` when the bytes are not a single valid JSON
        document, so the caller can omit the body rather than store a fragment.
        """
        if isinstance(raw, (bytes, bytearray)):
            text = bytes(raw).decode("utf-8")
        else:
            text = str(raw)
        if not text.strip():
            raise ValueError("empty JSON document")

        value = json.loads(text, parse_float=_RawNumber, parse_int=_RawNumber)
        counter = [0]
        redacted = self._redact_value(value, counter)
        return _dump_json(redacted).encode("utf-8"), counter[0]

    def _redact_value(self, value: Any, counter: List[int]) -> Any:
        if isinstance(value, dict):
            out: Dict[str, Any] = {}
            for key, item in value.items():
                if _matches(key, self.fields):
                    out[key] = REDACTED
                    counter[0] += 1
                else:
                    out[key] = self._redact_value(item, counter)
            return out
        if isinstance(value, list):
            return [self._redact_value(item, counter) for item in value]
        return value

    # -- free text ------------------------------------------------------

    def sanitize_error_text(self, text: Any, limit: int = MAX_ERROR_TEXT_BYTES) -> str:
        """Make exception text safe to store.

        Exception strings routinely embed a URL and therefore a credential, so
        they are never persisted verbatim.
        """
        cleaned = _clean_text(text)
        cleaned = _USERINFO_RE.sub(lambda m: m.group(1) + REDACTED + "@", cleaned)
        cleaned = _TEXT_SECRET_RE.sub(lambda m: m.group(1) + "=" + REDACTED, cleaned)
        cleaned = cleaned.strip()
        return _bound(cleaned, limit)


def _bound(text: str, limit: int) -> str:
    if len(text) <= limit:
        return text
    marker = "…"
    room = max(0, limit - len(marker.encode("utf-8")))
    encoded = text.encode("utf-8")[:room]
    while encoded:
        try:
            return encoded.decode("utf-8") + marker
        except UnicodeDecodeError:
            encoded = encoded[:-1]
    return marker


def _strip_unparseable_url(raw: str) -> str:
    """Keep only the leading part of a URL that could not be parsed."""
    for separator in ("?", "#"):
        index = raw.find(separator)
        if index >= 0:
            raw = raw[:index]
    return _bound(_USERINFO_RE.sub(lambda m: m.group(1) + REDACTED + "@", raw), MAX_URL_LENGTH)


# --------------------------------------------------------------- call sites

_INTERNAL_DIR = os.path.dirname(os.path.abspath(__file__))


def _call_site() -> str:
    """Describe the nearest frame outside the SDK and the standard library."""
    try:
        frame = sys._getframe(2)
    except ValueError:  # pragma: no cover - no Python frame available
        return ""
    while frame is not None:
        filename = frame.f_code.co_filename or ""
        if not _is_internal_frame(filename):
            base = os.path.basename(filename) or filename
            return _bound("%s:%d in %s" % (base, frame.f_lineno, frame.f_code.co_name),
                          MAX_CALL_SITE_LENGTH)
        frame = frame.f_back
    return ""


def _is_internal_frame(filename: str) -> bool:
    if not filename or filename.startswith("<"):
        return True
    absolute = os.path.abspath(filename) if not os.path.isabs(filename) else filename
    if absolute.startswith(_INTERNAL_DIR):
        return True
    lowered = filename.replace("\\", "/")
    return "/urllib/" in lowered or "/http/client" in lowered or lowered.endswith("http/client.py")


# ------------------------------------------------------------------- record

def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


class _Record:
    """Mutable state for one exchange, filled in as the exchange progresses."""

    def __init__(self, request_id: str, policy: str, method: str, url: str, call_site: str) -> None:
        self.request_id = request_id
        self.policy = policy
        self.method = method
        self.url = url
        self.call_site = call_site
        self.t0 = time.monotonic()
        self.t_headers: Optional[float] = None
        self.status: Optional[int] = None
        self.transport_error = ""
        self.transport_class = ""
        self.request_headers: List[Dict[str, str]] = []
        self.request_body: Optional[Dict[str, Any]] = None
        self.response_headers: List[Dict[str, str]] = []
        self.response_body: Optional[Dict[str, Any]] = None
        self.final_url = ""
        self.response_buffer = bytearray()
        self.response_eof = False
        self.response_overflow = False
        self.response_type = ""
        self.response_encoding = ""
        self.finished = False
        self.lock = threading.Lock()


# ------------------------------------------------------------------ capture

_STOP = object()


class Capture:
    """Records HTTP exchanges for one run and delivers them to the daemon."""

    def __init__(
        self,
        run_id: str,
        api_url: str,
        token: Optional[str],
        policy: str,
        redactor: Optional[Redactor] = None,
        queue_max: int = QUEUE_MAX_EVENTS,
    ) -> None:
        self.run_id = run_id
        self.api_url = api_url.rstrip("/")
        self.token = token
        self.policy = policy
        self.redactor = redactor or Redactor()
        self._queue: "deque[Tuple[Dict[str, Any], int]]" = deque()
        self._queue_max = queue_max
        self._lock = threading.Lock()
        self._wake = threading.Event()
        self._stopping = threading.Event()
        self._worker: Optional[threading.Thread] = None
        self._sequence = 0
        self._dropped_events = 0
        self._dropped_bytes = 0
        self._shut_down = False
        self._lifecycle_lock = threading.Lock()
        self._installed = False
        self._previous_sigterm: Any = None

    # -- lifecycle ------------------------------------------------------

    def start(self) -> None:
        """Install instrumentation and start the delivery worker."""
        from . import _urllib_capture

        _urllib_capture.install(self)
        self._installed = True
        self._worker = threading.Thread(
            target=self._run_worker, name="otter-capture", daemon=True
        )
        self._worker.start()
        self._install_signal_handler()

    def shutdown(self, note: str = "") -> None:
        """Deliver what is queued within the deadline and report completeness.

        Safe to call more than once, from any thread: the failure path, the
        success path and a signal all converge here.
        """
        with self._lifecycle_lock:
            if self._shut_down:
                return
            self._shut_down = True

        self._uninstall()
        self._stopping.set()
        self._wake.set()
        if self._worker is not None:
            self._worker.join(timeout=FLUSH_DEADLINE_SECONDS)

        with self._lock:
            remaining = len(self._queue)
            dropped_events = self._dropped_events + remaining
            dropped_bytes = self._dropped_bytes
            if remaining:
                self._queue.clear()

        finalization = FINALIZATION_COMPLETE
        if dropped_events > 0:
            finalization = FINALIZATION_INCOMPLETE
        self._deliver_summary(finalization, dropped_events, dropped_bytes, note)

    def _uninstall(self) -> None:
        if not self._installed:
            return
        from . import _urllib_capture

        _urllib_capture.uninstall()
        self._installed = False

    def _install_signal_handler(self) -> None:
        """Flush capture when the daemon terminates the child.

        The process is expected to die by SIGTERM: a normal exit would misreport
        how the run ended, so the default disposition is restored and the signal
        re-raised once capture is safely delivered.
        """
        if threading.current_thread() is not threading.main_thread():
            return
        try:
            self._previous_sigterm = signal.getsignal(signal.SIGTERM)
            signal.signal(signal.SIGTERM, self._on_sigterm)
        except (ValueError, OSError, AttributeError):  # pragma: no cover - exotic platforms
            self._previous_sigterm = None

    def _on_sigterm(self, signum: int, _frame: Any) -> None:  # pragma: no cover - signal path
        try:
            self.shutdown("the run was terminated; capture was flushed on the way out")
        except BaseException:
            pass
        try:
            signal.signal(signum, signal.SIG_DFL)
            os.kill(os.getpid(), signum)
        except BaseException:
            os._exit(128 + int(signum))

    # -- recording ------------------------------------------------------

    def begin(self, method: str, url: str, request_headers: Any = None, request_data: Any = None) -> _Record:
        """Record the start of an exchange and return its mutable record."""
        sanitized_url, url_redactions = self.redactor.sanitize_url(url)
        record = _Record(
            request_id=uuid.uuid4().hex,
            policy=self.policy,
            method=_bound(_clean_text(method) or "GET", MAX_METHOD_LENGTH),
            url=sanitized_url,
            call_site=_call_site(),
        )
        redactions = url_redactions

        if self.policy == POLICY_FULL:
            record.request_headers, header_redactions = self.redactor.sanitize_headers(
                list(request_headers or [])
            )
            redactions += header_redactions
            record.request_body = self._request_body_descriptor(request_data)

        self._emit({
            "kind": EVENT_STARTED,
            "request_id": record.request_id,
            "occurred_at": _now_iso(),
            "method": record.method,
            "url": record.url,
            "call_site": record.call_site,
            "request_headers": record.request_headers or None,
            "request_body": record.request_body,
        }, redactions)
        return record

    def note_response(self, record: _Record, status: Optional[int], headers: Any, final_url: str = "") -> None:
        """Record that response headers arrived."""
        with record.lock:
            if record.finished:
                return
            record.t_headers = time.monotonic()
            record.status = status
            if final_url:
                sanitized, redactions = self.redactor.sanitize_url(final_url)
                record.final_url = sanitized
            else:
                redactions = 0
            if self.policy == POLICY_FULL:
                record.response_headers, header_redactions = self.redactor.sanitize_headers(
                    list(headers or [])
                )
                redactions += header_redactions

        event = {
            "kind": EVENT_RESPONSE,
            "request_id": record.request_id,
            "occurred_at": _now_iso(),
            "status_code": status,
            "duration_to_headers_ms": self._ms(record.t_headers - record.t0),
            "response_headers": record.response_headers or None,
        }
        if record.final_url and record.final_url != record.url:
            event["final_url"] = record.final_url
        self._emit(event, redactions)

    def note_transport_error(self, record: _Record, exc: BaseException) -> None:
        """Record that the transport failed, without changing the exception."""
        with record.lock:
            record.transport_class = type(exc).__name__
            record.transport_error = self.redactor.sanitize_error_text(exc)
            record.finished = True

        self._emit({
            "kind": EVENT_COMPLETED,
            "request_id": record.request_id,
            "occurred_at": _now_iso(),
            "transport_error": record.transport_error,
            "transport_error_class": record.transport_class,
            "duration_total_ms": self._ms(time.monotonic() - record.t0),
        }, 0)

    def finish(self, record: _Record) -> None:
        """Record completion once the body has been consumed or closed."""
        with record.lock:
            if record.finished:
                return
            record.finished = True
            now = time.monotonic()
            record.response_body = self._response_body_descriptor(record)
            # The raw bytes are not needed once the descriptor exists. Keeping
            # only the sanitized value bounds what a long-lived response wrapper
            # holds on to.
            record.response_buffer = bytearray()
            to_headers = self._ms((record.t_headers or now) - record.t0)
            body_ms = self._ms(now - record.t_headers) if record.t_headers else 0
            total_ms = self._ms(now - record.t0)
            status = record.status
            transport_error = record.transport_error
            transport_class = record.transport_class

        event = {
            "kind": EVENT_COMPLETED,
            "request_id": record.request_id,
            "occurred_at": _now_iso(),
            "status_code": status,
            "transport_error": transport_error or None,
            "transport_error_class": transport_class or None,
            "duration_to_headers_ms": to_headers,
            "duration_body_ms": body_ms,
            "duration_total_ms": total_ms,
            "response_body": record.response_body,
        }
        # A redirect's final URL belongs to the finished exchange, so a reader
        # that only looks at the completed event still sees where it landed.
        if record.final_url and record.final_url != record.url:
            event["final_url"] = record.final_url
        self._emit(event, 0)

    # -- body descriptors -----------------------------------------------

    def _request_body_descriptor(self, data: Any) -> Optional[Dict[str, Any]]:
        if data is None:
            return None
        if not isinstance(data, (bytes, bytearray)):
            # A file-like or iterable body cannot be inspected without consuming
            # it, which would change what the request sends.
            return {"state": BODY_OMITTED, "reason": REASON_STREAM}
        raw = bytes(data)
        if not raw:
            return {"state": BODY_EMPTY}
        if len(raw) > MAX_BODY_BYTES:
            return {"state": BODY_OMITTED, "reason": REASON_OVERSIZED, "bytes_observed": len(raw)}
        try:
            sanitized, redactions = self.redactor.sanitize_json(raw)
        except (ValueError, UnicodeDecodeError, TypeError):
            return {"state": BODY_OMITTED, "reason": REASON_UNPARSEABLE, "bytes_observed": len(raw)}
        if len(sanitized) > MAX_BODY_BYTES:
            # Sanitising can grow a document slightly. Checking the encoded
            # result, not just the input, keeps one body from making the daemon
            # reject the batch that carries it.
            return {"state": BODY_OMITTED, "reason": REASON_OVERSIZED, "bytes_observed": len(raw)}
        return {
            "state": BODY_CAPTURED,
            "bytes_observed": len(raw),
            "json": json.loads(sanitized.decode("utf-8")),
            "redacted": redactions > 0,
            "redacted_count": redactions,
        }

    def _response_body_descriptor(self, record: _Record) -> Optional[Dict[str, Any]]:
        if record.policy != POLICY_FULL:
            return None
        if record.response_overflow:
            return {"state": BODY_OMITTED, "reason": REASON_OVERSIZED,
                    "bytes_observed": len(record.response_buffer)}
        if record.response_encoding:
            return {"state": BODY_OMITTED, "reason": REASON_ENCODED}
        if not record.response_eof:
            # The application read only a prefix: there is no complete value to
            # show, and a truncated JSON prefix must never be stored.
            return {"state": BODY_OMITTED, "reason": REASON_INCOMPLETE,
                    "bytes_observed": len(record.response_buffer)}
        raw = bytes(record.response_buffer)
        if not raw:
            return {"state": BODY_EMPTY}
        if record.response_type and not _is_json_content_type(record.response_type):
            return {"state": BODY_OMITTED, "reason": REASON_UNSUPPORTED,
                    "content_type": record.response_type, "bytes_observed": len(raw)}
        try:
            sanitized, redactions = self.redactor.sanitize_json(raw)
        except (ValueError, UnicodeDecodeError, TypeError):
            return {"state": BODY_OMITTED, "reason": REASON_UNPARSEABLE,
                    "content_type": record.response_type, "bytes_observed": len(raw)}
        if len(sanitized) > MAX_BODY_BYTES:
            return {"state": BODY_OMITTED, "reason": REASON_OVERSIZED,
                    "content_type": record.response_type, "bytes_observed": len(raw)}
        return {
            "state": BODY_CAPTURED,
            "content_type": record.response_type,
            "bytes_observed": len(raw),
            "json": json.loads(sanitized.decode("utf-8")),
            "redacted": redactions > 0,
            "redacted_count": redactions,
        }

    # -- queue and delivery ---------------------------------------------

    def _emit(self, event: Dict[str, Any], redactions: int) -> None:
        event = {key: value for key, value in event.items() if value is not None}
        with self._lock:
            if self._shut_down:
                return
            self._sequence += 1
            event["producer_seq"] = self._sequence
            encoded = json.dumps(event, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
            size = len(encoded)
            while len(self._queue) >= self._queue_max:
                dropped, dropped_size = self._queue.popleft()
                self._dropped_events += 1
                self._dropped_bytes += dropped_size
            self._queue.append((event, size))
        self._wake.set()

    def _take_batch(self) -> List[Dict[str, Any]]:
        batch: List[Dict[str, Any]] = []
        size = 0
        with self._lock:
            while self._queue and len(batch) < MAX_BATCH_EVENTS:
                event, event_size = self._queue[0]
                if batch and size + event_size > MAX_BATCH_BYTES:
                    break
                self._queue.popleft()
                batch.append(event)
                size += event_size
        return batch

    def _run_worker(self) -> None:  # pragma: no cover - thread body
        while True:
            self._wake.wait(timeout=0.5)
            self._wake.clear()
            while True:
                batch = self._take_batch()
                if not batch:
                    break
                self._deliver(batch)
            if self._stopping.is_set():
                # One last drain after the stop flag, then exit.
                if not self._take_batch():
                    return

    def _deliver(self, events: List[Dict[str, Any]]) -> bool:
        payload = {
            "schema_version": SCHEMA_VERSION,
            "policy": self.policy,
            "events": events,
        }
        return self._post(payload)

    def _deliver_summary(
        self, finalization: str, dropped_events: int, dropped_bytes: int, note: str
    ) -> None:
        payload: Dict[str, Any] = {
            "schema_version": SCHEMA_VERSION,
            "policy": self.policy,
            "dropped_events": dropped_events,
            "dropped_bytes": dropped_bytes,
            "finalization": finalization,
        }
        if note:
            payload["note"] = _bound(_clean_text(note), 512)
        self._post(payload)

    def _post(self, payload: Dict[str, Any]) -> bool:
        body = json.dumps(payload, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
        path = "/v1/runs/%s/requests/events" % self.run_id
        host, port, secure = _split_api_url(self.api_url)
        if host is None:
            return False

        last_error: Optional[BaseException] = None
        for attempt in range(DELIVERY_ATTEMPTS):
            connection: Any = None
            try:
                if secure:
                    connection = http.client.HTTPSConnection(
                        host, port, timeout=DELIVERY_TIMEOUT_SECONDS)
                else:
                    connection = http.client.HTTPConnection(
                        host, port, timeout=DELIVERY_TIMEOUT_SECONDS)
                headers = {"Content-Type": "application/json", "Accept": "application/json"}
                if self.token:
                    headers["Authorization"] = "Bearer %s" % self.token
                connection.request("POST", path, body=body, headers=headers)
                response = connection.getresponse()
                response.read()
                status = response.status
                if 200 <= status < 300:
                    return True
                # 4xx means this batch will never be accepted: retrying cannot
                # help, and the daemon has already recorded the loss.
                if 400 <= status < 500:
                    return False
                last_error = RuntimeError("capture delivery returned HTTP %d" % status)
            except BaseException as exc:  # never let delivery break the caller
                last_error = exc
            finally:
                if connection is not None:
                    try:
                        connection.close()
                    except BaseException:
                        pass
            if attempt + 1 < DELIVERY_ATTEMPTS:
                time.sleep(0.05 * (attempt + 1))
        _ = last_error
        return False

    @staticmethod
    def _ms(seconds: Optional[float]) -> int:
        if seconds is None or seconds < 0:
            return 0
        return int(round(seconds * 1000))


def _is_json_content_type(content_type: str) -> bool:
    lowered = (content_type or "").split(";", 1)[0].strip().lower()
    if lowered in ("application/json", "text/json"):
        return True
    return lowered.startswith("application/") and lowered.endswith("+json")


def _split_api_url(api_url: str) -> Tuple[Optional[str], Optional[int], bool]:
    try:
        parts = urlsplit(api_url)
    except ValueError:
        return None, None, False
    if not parts.hostname:
        return None, None, False
    secure = parts.scheme == "https"
    port = parts.port
    if port is None:
        port = 443 if secure else 80
    return parts.hostname, port, secure


# ---------------------------------------------------------------- module api

_ACTIVE: Optional[Capture] = None
_ACTIVE_LOCK = threading.Lock()


def install_from_environment(environ: Optional[Dict[str, str]] = None) -> Optional[Capture]:
    """Install capture when the daemon asked for it.

    Capture is off unless ``OTTER_CAPTURE_POLICY`` names metadata or full, so a
    standalone run pays nothing for this module.
    """
    env = os.environ if environ is None else environ
    policy = (env.get("OTTER_CAPTURE_POLICY") or "").strip().lower()
    if policy not in (POLICY_METADATA, POLICY_FULL):
        return None
    run_id = (env.get("OTTER_RUN_ID") or "").strip()
    api_url = (env.get("OTTER_API_URL") or "").strip()
    if not run_id or not api_url:
        return None
    token = (env.get("OTTER_STATE_TOKEN") or "").strip() or None

    global _ACTIVE
    with _ACTIVE_LOCK:
        if _ACTIVE is not None:
            return _ACTIVE
        capture = Capture(run_id=run_id, api_url=api_url, token=token, policy=policy)
        try:
            capture.start()
        except BaseException:
            # Instrumentation is diagnostic: a failure to install it must leave
            # the integration running normally.
            return None
        _ACTIVE = capture
        return capture


def flush_before_exit(note: str = "") -> None:
    """Deliver pending capture before the process leaves.

    Called from both the success path and, crucially, before ``os._exit`` on the
    failure path, where no ``atexit`` handler would run.
    """
    capture = _ACTIVE
    if capture is None:
        return
    try:
        capture.shutdown(note)
    except BaseException:
        # A capture failure must never change an exit status.
        pass
