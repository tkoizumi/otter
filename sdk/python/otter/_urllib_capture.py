"""urllib instrumentation for Otter HTTP capture.

The whole adapter is one wrapper around ``urllib.request.OpenerDirector.open``.
That single seam covers ``urllib.request.urlopen`` and every custom opener that
uses the base implementation, which is what v1 promises: coverage of the
standard library transport, not "all HTTP".

Two behaviours matter more than coverage:

* **Responses are observed, not drained.** Bytes are collected as application
  code reads them, so a stream that is never read is never consumed and a
  prefix that is read is reported as incomplete rather than invented.
* **Exceptions pass through unchanged.** Transport errors and ``HTTPError`` are
  re-raised as the very same object, with the error's readable body still
  readable exactly once.
"""

import socket
import threading
import urllib.error
import urllib.request
from typing import Any, List, Optional, Tuple

from . import _capture

__all__ = ["install", "uninstall"]

_install_lock = threading.Lock()
_original_open: Any = None
#: The active capture, or None. Deliberately not named ``_capture``: that name
#: belongs to the module, and an annotated assignment evaluates its annotation
#: after binding the value, so reusing the name would shadow the module.
_active_capture: Optional[Any] = None

#: Depth of nested opener calls on this thread. urllib follows redirects by
#: calling the opener again; those inner hops belong to the outer exchange, whose
#: final URL already reflects them.
_depth = threading.local()


def install(capture: _capture.Capture) -> None:
    """Start capturing the standard library transport."""
    global _original_open, _active_capture
    with _install_lock:
        _active_capture = capture
        if _original_open is None:
            _original_open = urllib.request.OpenerDirector.open
            urllib.request.OpenerDirector.open = _capturing_open


def uninstall() -> None:
    """Stop capturing and restore the original opener."""
    global _original_open, _active_capture
    with _install_lock:
        if _original_open is not None:
            urllib.request.OpenerDirector.open = _original_open
            _original_open = None
        _active_capture = None


def _capturing_open(self: Any, fullurl: Any, data: Any = None,
                    timeout: Any = socket._GLOBAL_DEFAULT_TIMEOUT) -> Any:
    original = _original_open
    capture = _active_capture
    if original is None or capture is None or _capture.is_suppressed():
        return original(self, fullurl, data, timeout)

    depth = getattr(_depth, "value", 0)
    if depth > 0:
        return original(self, fullurl, data, timeout)

    request = _as_request(fullurl, data)
    if request is None:
        return original(self, fullurl, data, timeout)

    record = capture.begin(
        _request_method(request),
        request.full_url,
        _request_headers(request),
        getattr(request, "data", None),
    )

    _depth.value = depth + 1
    try:
        response = original(self, fullurl, data, timeout)
    except urllib.error.HTTPError as exc:
        # An unsuccessful response is not a transport failure: it has a status,
        # headers and a body the caller is expected to read.
        _note_response(capture, record, exc)
        _observe_error_body(capture, record, exc)
        raise
    except BaseException as exc:
        capture.note_transport_error(record, exc)
        raise
    finally:
        _depth.value = depth

    _note_response(capture, record, response)
    return _CapturedResponse(capture, record, response)


def _as_request(fullurl: Any, data: Any) -> Optional[urllib.request.Request]:
    if isinstance(fullurl, urllib.request.Request):
        return fullurl
    try:
        return urllib.request.Request(fullurl, data=data)
    except BaseException:
        return None


def _request_method(request: urllib.request.Request) -> str:
    try:
        return request.get_method()
    except BaseException:
        return "GET"


def _request_headers(request: urllib.request.Request) -> List[Tuple[str, str]]:
    pairs: List[Tuple[str, str]] = []
    try:
        for name, value in request.header_items():
            pairs.append((name, value))
    except BaseException:
        pass
    return pairs


def _note_response(capture: _capture.Capture, record: Any, response: Any) -> None:
    status = getattr(response, "status", None)
    if not isinstance(status, int):
        code = getattr(response, "code", None)
        status = code if isinstance(code, int) else None
    pairs = _response_headers(response)
    final_url = _safe_geturl(response)

    # Content metadata drives the body decision and must come from the raw
    # headers, before any redaction.
    content_type = _header_value(pairs, "content-type")
    content_encoding = _header_value(pairs, "content-encoding")
    with record.lock:
        record.response_type = content_type
        record.response_encoding = content_encoding

    capture.note_response(record, status, pairs, final_url)


def _response_headers(response: Any) -> List[Tuple[str, str]]:
    pairs: List[Tuple[str, str]] = []
    try:
        headers = getattr(response, "headers", None)
        if headers is not None:
            for name, value in headers.items():
                pairs.append((name, value))
        if not pairs:
            info = getattr(response, "info", None)
            if callable(info):
                for name, value in info().items():
                    pairs.append((name, value))
    except BaseException:
        pass
    return pairs


def _header_value(pairs: List[Tuple[str, str]], name: str) -> str:
    wanted = name.lower()
    for header, value in pairs:
        if header.lower() == wanted:
            return value or ""
    return ""


def _safe_geturl(response: Any) -> str:
    try:
        geturl = getattr(response, "geturl", None)
        if callable(geturl):
            return str(geturl() or "")
    except BaseException:
        pass
    try:
        return str(getattr(response, "url", "") or "")
    except BaseException:
        return ""


def _observe(capture: _capture.Capture, record: Any, data: Any, eof: bool) -> None:
    """Record bytes the application just consumed."""
    if not data and not eof:
        return
    finish_now = False
    with record.lock:
        # A metadata recording never keeps payload bytes. Buffering them would
        # retain exactly the data the policy promised not to store, so the bytes
        # are counted and dropped rather than accumulated.
        if data and record.policy == _capture.POLICY_FULL:
            chunk = bytes(data)
            if len(record.response_buffer) + len(chunk) > _capture.MAX_BODY_BYTES:
                record.response_overflow = True
            else:
                record.response_buffer.extend(chunk)
        if eof:
            record.response_eof = True
            finish_now = True
    if finish_now:
        capture.finish(record)


def _read_eof(amount: Any, data: Any) -> bool:
    """Report whether this read reached the end of the body."""
    if amount is None or (isinstance(amount, int) and amount < 0):
        return True
    if isinstance(amount, int) and amount == 0:
        return False
    return not data


class _CapturedResponse:
    """Delegates to the real response while observing what is consumed.

    Every read path the standard library offers is preserved, and anything not
    explicitly implemented is forwarded, so a caller cannot tell the difference
    beyond a slightly different type.
    """

    def __init__(self, capture: _capture.Capture, record: Any, response: Any) -> None:
        self._capture = capture
        self._record = record
        self._response = response

    # -- reading --------------------------------------------------------

    def read(self, amount: Any = None) -> bytes:
        data = self._response.read(amount)
        _observe(self._capture, self._record, data, _read_eof(amount, data))
        return data

    def readline(self, limit: int = -1) -> bytes:
        data = self._response.readline(limit)
        _observe(self._capture, self._record, data, not data)
        return data

    def readinto(self, buffer: Any) -> Optional[int]:
        count = self._response.readinto(buffer)
        size = count or 0
        _observe(self._capture, self._record, buffer[:size], size == 0)
        return count

    def readlines(self, hint: int = -1) -> List[bytes]:
        lines = self._response.readlines(hint)
        for line in lines:
            _observe(self._capture, self._record, line, not line)
        return lines

    def __iter__(self) -> Any:
        while True:
            line = self.readline()
            if not line:
                return
            yield line

    # -- lifecycle ------------------------------------------------------

    def __enter__(self) -> "_CapturedResponse":
        self._response.__enter__()
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> Any:
        result = self._response.__exit__(exc_type, exc, tb)
        self._finish()
        return result

    def close(self) -> None:
        try:
            self._response.close()
        finally:
            self._finish()

    def _finish(self) -> None:
        self._capture.finish(self._record)

    def __getattr__(self, name: str) -> Any:
        # Only reached for attributes not defined above, which is how status,
        # headers, reason, geturl, fileno and everything else stay available.
        return getattr(self._response, name)


# ------------------------------------------------------------- error bodies

def _observe_error_body(capture: _capture.Capture, record: Any, exc: urllib.error.HTTPError) -> None:
    """Observe an HTTPError body as the caller reads it.

    The body is single-read, so it is never touched here: ``read`` and
    ``readline`` are wrapped on the exception instance instead, and completion is
    recorded when the body reaches EOF or is closed. An error whose body the
    caller never reads stays incomplete, which is the truth.
    """
    original_read = getattr(exc, "read", None)
    original_readline = getattr(exc, "readline", None)
    original_close = getattr(exc, "close", None)

    if callable(original_read):
        def read(amount: Any = None, _original: Any = original_read) -> bytes:
            data = _original(amount)
            _observe(capture, record, data, _read_eof(amount, data))
            return data
        try:
            exc.read = read  # type: ignore[attr-defined]
        except BaseException:
            pass

    if callable(original_readline):
        def readline(limit: int = -1, _original: Any = original_readline) -> bytes:
            data = _original(limit)
            _observe(capture, record, data, not data)
            return data
        try:
            exc.readline = readline  # type: ignore[attr-defined]
        except BaseException:
            pass

    if callable(original_close):
        def close(_original: Any = original_close) -> Any:
            try:
                return _original()
            finally:
                capture.finish(record)
        try:
            exc.close = close  # type: ignore[attr-defined]
        except BaseException:
            pass
